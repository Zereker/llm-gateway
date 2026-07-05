package middleware

import (
	"bytes"
	"context"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/zereker/llm-gateway/pkg/embed"
	"github.com/zereker/llm-gateway/pkg/metric"
)

// SemanticCache 是响应缓存的进阶:按请求 prompt 的**向量相似度**命中,而非字节精确
// 匹配——paraphrase / 措辞不同但语义相同的请求也能共享缓存。
//
// 流程(命中/回写/usage 逻辑跟精确缓存共用 helper,继承 H1 毒化防线 + M4 流式兜底):
//  1. 抽 prompt(messages 内容 + system)→ Embedder 转向量
//  2. 在 (protocol|model) 命名空间里找 cosine 相似度 ≥ threshold 的历史条目 → 命中返回
//  3. 未命中:tee 响应,干净 200 就连同向量存起来
//
// **安全默认同精确缓存**:非流式 + temperature=0;X-Gateway-Cache off/on 覆盖。
// Embed 失败 → 不缓存、正常放行(不因 embedder 报错中断请求)。
// **注意**:语义查找需先对 prompt 取向量,embedder.Embed 在 c.Next() 前**同步**
// 调用——embedder 慢会给 eligible 请求叠加最多一个 embed 超时的延迟(见
// OpenAIEmbedder client.Timeout)。部署时 embedder 端点须低延迟高可用,否则
// 语义缓存反而拖累 p99;可用 X-Gateway-Cache=off 或 temperature≠0 绕开。
// store / embedder 任一 nil → 整个中间件 no-op。
func SemanticCache(store SemanticCacheStore, embedder embed.Embedder, threshold float64, ttl time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		if store == nil || embedder == nil {
			c.Next()
			return
		}
		rc := GetRequestContext(c)
		mode := strings.ToLower(strings.TrimSpace(c.GetHeader(HeaderGatewayCache)))
		if mode == "off" || rc.Envelope == nil || rc.ModelService == nil {
			c.Next()
			return
		}
		stream, deterministic := analyzeBody(rc.Envelope.RawBytes)
		if stream {
			c.Next()
			return
		}
		if mode != "on" && !deterministic {
			metric.Inc(metric.ResponseCacheTotal, "result", "bypass")
			c.Next()
			return
		}
		prompt := extractPrompt(rc.Envelope.RawBytes)
		if prompt == "" {
			c.Next()
			return
		}

		ctx := c.Request.Context()
		vec, err := embedder.Embed(ctx, prompt)
		if err != nil {
			// embedder 抖动 → 不缓存,放行(不阻塞请求)。
			metric.Inc(metric.ResponseCacheTotal, "result", "embed_error")
			c.Next()
			return
		}
		ns := rc.Envelope.SourceProtocol.String() + "|" + rc.ModelService.Model

		if cached, ok := store.Lookup(ctx, ns, vec, threshold); ok {
			metric.Inc(metric.ResponseCacheTotal, "result", "semantic_hit")
			writeCacheHit(c, rc, cached)
			return
		}

		metric.Inc(metric.ResponseCacheTotal, "result", "semantic_miss")
		tw := &teeWriter{ResponseWriter: c.Writer, buf: &bytes.Buffer{}}
		c.Writer = tw
		c.Next()

		if resp, ok := cacheableResponse(tw, rc); ok {
			store.Store(ctx, ns, vec, resp, ttl)
			metric.Inc(metric.ResponseCacheTotal, "result", "semantic_store")
		}
	}
}

// SemanticCacheStore 语义缓存存储端口（Redis 实现见 pkg/respcache）。
type SemanticCacheStore interface {
	// Lookup 在 namespace 里找与 vec 相似度 ≥ threshold 的条目；找不到返回 (_, false)。
	Lookup(ctx context.Context, namespace string, vec []float32, threshold float64) (CachedResponse, bool)
	// Store 把 vec + 响应存进 namespace（带 TTL；实现自行做条目上限）。
	Store(ctx context.Context, namespace string, vec []float32, resp CachedResponse, ttl time.Duration)
}

// extractPrompt 从请求 body 抽出用于 embedding 的文本,覆盖三种客户端入口:
//   - OpenAI / Anthropic ChatCompletion：messages 各 content + 顶层 system
//   - OpenAI Responses：顶层 input（string 或 array）+ instructions
//
// content/input 是数组(多模态)时取其 JSON 串——够 embedding 用。Responses body 走
// RawBytes(翻译前客户端原文)到这里,不覆盖它语义缓存会对 Responses 客户端静默失效。
func extractPrompt(body []byte) string {
	var sb strings.Builder
	gjson.GetBytes(body, "messages.#.content").ForEach(func(_, v gjson.Result) bool {
		sb.WriteString(v.String())
		sb.WriteByte('\n')
		return true
	})
	if s := gjson.GetBytes(body, "system"); s.Exists() {
		sb.WriteString(s.String())
		sb.WriteByte('\n')
	}
	// Responses 协议：input 可能是 string 或 array of items。
	if in := gjson.GetBytes(body, "input"); in.Exists() {
		if in.IsArray() {
			in.ForEach(func(_, v gjson.Result) bool {
				sb.WriteString(v.String())
				sb.WriteByte('\n')
				return true
			})
		} else {
			sb.WriteString(in.String())
			sb.WriteByte('\n')
		}
	}
	if ins := gjson.GetBytes(body, "instructions"); ins.Exists() {
		sb.WriteString(ins.String())
	}
	return strings.TrimSpace(sb.String())
}
