package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/metric"
)

// ResponseCache 是响应缓存中间件——命中直接返回缓存,跳过 M7 调度(不打上游),省钱降
// 延迟。放在 M6 Limit 之后、M7 Schedule 之前:命中仍计 RPM(缓存后 Limit 已扣),但零
// 上游成本。
//
// **默认只缓存确定性请求**:非流式 + temperature=0。非确定请求(temperature≠0 / 缺省)
// 缓存会返回旧结果、行为诡异,所以默认跳过;客户端可用 X-Gateway-Cache: on 强制缓存
// (自负风险),off 完全绕过。流式**从不**缓存(v1)。
//
// **key** = SHA256(sourceProtocol | canonical model | 请求 body)。同协议 + 同 model +
// 同 body → 同响应字节。跨账号共享(响应是 model 的输出,与账号无关),命中率更高。
//
// **usage**:命中时透传缓存里的 usage(Source=cache),M10 照常出 usage 事件、M6 照常
// 后扣 TPM——下游按 source=cache 可零成本计费。
//
// store == nil(未配置)时整个中间件是 no-op,不影响链。
func ResponseCache(store ResponseCacheStore, ttl time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		if store == nil {
			c.Next()
			return
		}
		rc := GetRequestContext(c)
		mode := c.GetHeader(HeaderGatewayCache)
		if mode == "off" || rc.Envelope == nil || rc.ModelService == nil {
			c.Next()
			return
		}

		stream, deterministic := analyzeBody(rc.Envelope.RawBytes)
		if stream {
			c.Next() // 流式从不缓存
			return
		}
		if mode != "on" && !deterministic {
			metric.Inc(metric.ResponseCacheTotal, "result", "bypass")
			c.Next() // 默认只缓存确定性请求
			return
		}

		ctx := c.Request.Context()
		key := cacheKey(rc.Envelope.SourceProtocol, rc.ModelService.Model, rc.Envelope.RawBytes)

		// 命中:写缓存响应 + 透传 usage,abort 跳过 M7（M6-post/M10 在洋葱返程仍跑）。
		if cached, ok := store.Get(ctx, key); ok {
			metric.Inc(metric.ResponseCacheTotal, "result", "hit")
			ct := cached.ContentType
			if ct == "" {
				ct = "application/json; charset=utf-8"
			}
			c.Header(HeaderGatewayCache, "hit")
			c.Data(cached.StatusCode, ct, cached.Body)
			if cached.Usage != nil {
				u := *cached.Usage
				u.Source = domain.UsageSourceCache
				rc.Usage = &u
			}
			c.Abort()
			return
		}

		// 未命中:tee response,成功则回写缓存。
		metric.Inc(metric.ResponseCacheTotal, "result", "miss")
		tw := &teeWriter{ResponseWriter: c.Writer, buf: &bytes.Buffer{}}
		c.Writer = tw
		c.Next()

		if tw.Status() == 200 && tw.buf.Len() > 0 {
			store.Set(ctx, key, CachedResponse{
				StatusCode:  200,
				ContentType: tw.Header().Get("Content-Type"),
				Body:        tw.buf.Bytes(),
				Usage:       rc.Usage,
			}, ttl)
			metric.Inc(metric.ResponseCacheTotal, "result", "store")
		}
	}
}

// ResponseCacheStore 响应缓存存储端口（Redis 实现见 cmd 装配点）。
type ResponseCacheStore interface {
	Get(ctx context.Context, key string) (CachedResponse, bool)
	Set(ctx context.Context, key string, resp CachedResponse, ttl time.Duration)
}

// CachedResponse 缓存的一次完整非流式响应。
type CachedResponse struct {
	StatusCode  int
	ContentType string
	Body        []byte
	Usage       *domain.Usage
}

// cacheKey = SHA256(protocol | model | body) 的 hex。
func cacheKey(proto domain.Protocol, model string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(proto.String()))
	h.Write([]byte{0})
	h.Write([]byte(model))
	h.Write([]byte{0})
	h.Write(body)
	return "resp:" + hex.EncodeToString(h.Sum(nil))
}

// analyzeBody 从请求 body 解析 (stream, deterministic)。
//
// deterministic = temperature 显式为 0（缺省 temperature 各家默认多为 1，视为非确定）。
// 解析失败一律当 (stream=false, deterministic=false)——保守:默认不缓存。
func analyzeBody(body []byte) (stream, deterministic bool) {
	var probe struct {
		Stream      bool     `json:"stream"`
		Temperature *float64 `json:"temperature"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return false, false
	}
	deterministic = probe.Temperature != nil && *probe.Temperature == 0
	return probe.Stream, deterministic
}

// teeWriter 包 gin.ResponseWriter，把写出的 body 同时抄一份到 buf（缓存回写用）。
type teeWriter struct {
	gin.ResponseWriter
	buf *bytes.Buffer
}

func (w *teeWriter) Write(b []byte) (int, error) {
	w.buf.Write(b)
	return w.ResponseWriter.Write(b)
}

func (w *teeWriter) WriteString(s string) (int, error) {
	w.buf.WriteString(s)
	return w.ResponseWriter.WriteString(s)
}
