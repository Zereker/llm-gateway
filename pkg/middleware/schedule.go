package middleware

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/adapter"
	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/schedule"
	"github.com/zereker-labs/ai-gateway/pkg/translator"
)

// chunkBufPool 复用 stream forward 用的 4KiB read buffer。
//
// 高 QPS 流式场景下每请求 make([]byte, 4096) 会显著增加 GC 压力（一个
// 长流式请求只 alloc 一次，所以池化的本质是减少 short-lived buffer churn）。
// pool 存 *[]byte（slice header pointer），避免 sync.Pool 用 interface{} 时的
// 内存分配（Go FAQ 推荐做法）。
//
// buffer size 4KiB 跟原代码一致：典型 SSE chunk 几百字节到 1KiB；4KiB 一次
// Read 通常能拿一两个 chunk，刚好。
var chunkBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 4096)
		return &b
	},
}

// ScheduleDeps M7 Schedule middleware 的依赖。
//
// **v0.5 完整版**（pkg/schedule 接管）：
//   - Scheduler 负责"选 endpoint + cooldown + 跨 EP 重试 + Filter 链"
//   - 本 middleware 只负责"调上游 + 流式 chunk → translator → client + report 结果回 Scheduler"
type ScheduleDeps struct {
	Scheduler  schedule.Scheduler                  // pkg/schedule 完整选路 + cooldown + 重试
	GetFactory func(vendor string) adapter.Factory // nil = 使用 adapter.Get
	HTTPClient *http.Client                        // nil = 使用 http.DefaultClient
}

// Schedule 是 M7：调 Scheduler 选 endpoint → 调 Adapter → 流式回写客户端。
//
// **driver loop 模式**：
//   1. BeginSelection 拿候选 + 选路状态机
//   2. for { ep := Pick(); 调 endpoint; classify result; Report; if !retryable break }
//   3. Decisions 写 rc.SchedulingDecision
//
// **响应已开始写后**，rc.Error 仍可设置但 M9 无法覆盖（流式架构固有约束）。
func Schedule(deps ScheduleDeps) gin.HandlerFunc {
	httpc := deps.HTTPClient
	if httpc == nil {
		httpc = http.DefaultClient
	}
	getFactory := deps.GetFactory
	if getFactory == nil {
		getFactory = adapter.Get
	}

	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		if rc.Envelope == nil || rc.ModelService == nil {
			abort(c, 500, domain.ErrUnknown, "internal: M3/M5 did not run before M7")
			return
		}

		// 1. find translator
		srcProto := rc.Envelope.SourceProtocol
		// vendor → factory → metadata.NativeProtocol；不同 ep 的 vendor 可能不同，
		// 但同 model 的 candidates 一般同 vendor。先在选完 endpoint 后查 factory。
		// 这里先记下 srcProto；factory 在 driver loop 内取（每个 ep 对应自己的 factory）。

		// 2. 估 TPM cost（M6 应该已经填了；这里 ensure）
		var rawBody []byte
		if rc.Envelope != nil {
			rawBody = rc.Envelope.RawBytes
		}
		tpmCost := EnsureTPMEstimate(rc, rawBody)

		// 3. 解析 X-Gateway-Max-Attempts header 覆盖
		maxAttemptsOverride := 0
		if hdr := c.GetHeader(HeaderGatewayMaxAttempts); hdr != "" {
			if v, err := strconv.Atoi(hdr); err == nil && v > 0 {
				maxAttemptsOverride = v
			}
		}

		// 3b. 解析 X-Gateway-Fallback-Models header（L3 fallback 链；逗号分隔 model 名）
		var fallbackModels []string
		if hdr := c.GetHeader(HeaderGatewayFallbackModels); hdr != "" {
			for _, m := range strings.Split(hdr, ",") {
				if m = strings.TrimSpace(m); m != "" {
					fallbackModels = append(fallbackModels, m)
				}
			}
		}

		// 4. BeginSelection
		// PrefixKey 截断到 4KiB 上限，避免大 body 影响哈希计算（4KiB 对 prefix-cache 已足够区分）
		const prefixKeyMax = 4 * 1024
		var prefixKey []byte
		if n := len(rawBody); n > 0 {
			if n > prefixKeyMax {
				n = prefixKeyMax
			}
			prefixKey = rawBody[:n]
		}
		sel, err := deps.Scheduler.BeginSelection(rc.Ctx, &schedule.Request{
			Model:               rc.ModelService.Model,
			Group:               rc.Identity.Group,
			TPMCost:             tpmCost,
			MaxAttemptsOverride: maxAttemptsOverride,
			FallbackModels:      fallbackModels,
			PrefixKey:           prefixKey,
		})
		if err != nil {
			abort(c, 503, domain.ErrTransient, err.Error())
			return
		}
		defer sel.Done()

		// 5. driver loop：Pick → call → Report → 重试
		responseStarted := false
		for {
			ep := sel.Pick()
			if ep == nil {
				if !responseStarted {
					decisions := sel.Decisions()
					abort(c, 503, domain.ErrTransient,
						fmt.Sprintf("schedule: no endpoint succeeded after %d attempts", len(decisions)))
				}
				break
			}
			rc.Endpoint = ep

			// 选完 ep 后，把对应 endpoint 维度 TPM bucket key 追加给 M10 调账
			if rc.RateLimit != nil {
				rc.RateLimit.TPMBucketKeys = append(rc.RateLimit.TPMBucketKeys,
					schedule.EndpointTPMBucketKeys(ep)...)
			}

			// 取 factory + translator
			factory := getFactory(ep.Vendor)
			if factory == nil {
				sel.Report(ep, schedule.Result{
					Class:  schedule.ClassPermanent,
					Reason: "no adapter registered for vendor: " + ep.Vendor,
				})
				continue
			}
			tgtProto := factory.Metadata().NativeProtocol
			trans := translator.Find(srcProto, tgtProto)
			if trans == nil {
				sel.Report(ep, schedule.Result{
					Class:  schedule.ClassPermanent,
					Reason: fmt.Sprintf("no translator for %s → %s", srcProto, tgtProto),
				})
				continue
			}

			// 翻译请求（可能因 stream=true 等 v0.5 不支持原因失败）
			upstreamBody, err := trans.TranslateRequest(rawBody)
			if err != nil {
				sel.Report(ep, schedule.Result{
					Class:  schedule.ClassInvalid,
					Reason: "translate request: " + err.Error(),
				})
				// Invalid 不重试（同样请求换 endpoint 也会失败）
				if !responseStarted {
					abort(c, 400, domain.ErrInvalid, "translate request: "+err.Error())
				}
				return
			}

			// 构造 session + 上游请求
			start := time.Now()
			sess, err := factory.NewSession(rc.Ctx, ep, rc.Envelope)
			if err != nil {
				sel.Report(ep, schedule.Result{
					Class:   schedule.ClassTransient,
					Reason:  "NewSession: " + err.Error(),
					Latency: time.Since(start),
				})
				continue
			}
			req, err := sess.BuildRequest(upstreamBody)
			if err != nil {
				_ = sess.Close()
				sel.Report(ep, schedule.Result{
					Class:   schedule.ClassPermanent,
					Reason:  "BuildRequest: " + err.Error(),
					Latency: time.Since(start),
				})
				continue
			}
			req = req.WithContext(rc.Ctx)

			// 调上游
			resp, err := httpc.Do(req)
			if err != nil {
				_ = sess.Close()
				sel.Report(ep, schedule.Result{
					Class:   schedule.ClassTransient,
					Reason:  "upstream call: " + err.Error(),
					Latency: time.Since(start),
				})
				continue
			}

			// 按 status 分类（在写 client 前判，无 retry 才 commit status）
			class := classifyHTTPStatus(resp.StatusCode)
			// Adapter Classifier 细化：非 2xx 时让 vendor 自己解释 error body
			// （eg OpenAI 区分 insufficient_quota vs rate_limit；Anthropic overloaded_error → capacity）
			if class != schedule.ClassSuccess {
				if cls, ok := factory.(adapter.Classifier); ok {
					peeked := peekBodyForClassify(resp)
					if refined := cls.Classify(resp.StatusCode, peeked); refined != nil {
						class = adapterErrToScheduleClass(refined.Class, class)
					}
				}
			}
			if class.IsRetryable() && class != schedule.ClassSuccess {
				// transient / capacity / permanent → 关 resp 试下一个
				_ = resp.Body.Close()
				_ = sess.Close()
				sel.Report(ep, schedule.Result{
					Class:    class,
					HTTPCode: resp.StatusCode,
					Reason:   fmt.Sprintf("upstream status %d", resp.StatusCode),
					Latency:  time.Since(start),
				})
				continue
			}

			// 成功 or invalid（4xx 客户端错）：commit status + forward body
			responseStarted = true
			copyHeaders(c.Writer.Header(), resp.Header)
			c.Writer.WriteHeader(resp.StatusCode)
			c.Writer.WriteHeaderNow()

			// chunk → translator handler → client
			handler := trans.NewResponseHandler()
			bufPtr := chunkBufPool.Get().(*[]byte)
			buf := *bufPtr
			var feedErr error
			for {
				n, rerr := resp.Body.Read(buf)
				if n > 0 {
					out, herr := handler.Feed(buf[:n])
					if herr != nil {
						feedErr = herr
						break
					}
					if len(out) > 0 {
						_, _ = c.Writer.Write(out)
						c.Writer.Flush()
					}
				}
				if rerr == io.EOF {
					break
				}
				if rerr != nil {
					feedErr = rerr
					break
				}
			}
			chunkBufPool.Put(bufPtr)
			finalOut, usage, fErr := handler.Flush()
			if len(finalOut) > 0 {
				_, _ = c.Writer.Write(finalOut)
				c.Writer.Flush()
			}
			rc.Usage = usage
			_ = resp.Body.Close()
			_ = sess.Close()

			finalClass := class
			if feedErr != nil || fErr != nil {
				finalClass = schedule.ClassTransient
				rc.Error = &domain.AdapterError{
					Class:   domain.ErrTransient,
					Message: "translator: " + firstNonEmpty(feedErrMsg(feedErr), feedErrMsg(fErr)),
				}
			}
			sel.Report(ep, schedule.Result{
				Class:    finalClass,
				HTTPCode: resp.StatusCode,
				Latency:  time.Since(start),
			})

			// 不论成功 / 失败，response 已开始：不能再 retry
			break
		}

		// 写完整尝试链给 M10 trace
		if decisions := sel.Decisions(); len(decisions) > 0 {
			rc.SchedulingDecision = &domain.SchedulingDecision{
				Attempts: convertDecisions(decisions),
			}
		}
	}
}

// peekBodyForClassify 错误响应时小量读 body（≤4KiB）让 adapter Classifier 解析；
// 读完替换 resp.Body 让后续 retryable check 之外的路径还能读到完整 body。
//
// 错误 body 通常都很小（OpenAI/Anthropic 都是几百字节 JSON）；4KiB 上限保护
// 异常巨大的 body 不会爆内存。
//
// 读取失败（已经被 reader 消费 / 超时）：返回 nil，让 Classifier fallback 到 status-only。
func peekBodyForClassify(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	const peekMax = 4 * 1024
	buf := make([]byte, peekMax)
	n, _ := io.ReadFull(io.LimitReader(resp.Body, peekMax), buf)
	if n == 0 {
		return nil
	}
	peeked := buf[:n]
	// 把读出的字节拼回去，让后续如果要 forward body 还能拿到
	resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(peeked), resp.Body))
	return peeked
}

// adapterErrToScheduleClass domain.ErrorClass → schedule.ErrorClass。
//
// 不能 1:1 映射的字段：domain.ErrUnknown 兜底到原 fallback class（HTTP-status 推导的那个），
// 因为 schedule.ClassUnknown 在 IsRetryable 上是 true（会被 retry），而 ErrUnknown 在
// adapter 看应该是"我不知道"——保留原 HTTP-status 分类更安全。
func adapterErrToScheduleClass(c domain.ErrorClass, fallback schedule.ErrorClass) schedule.ErrorClass {
	switch c {
	case domain.ErrInvalid:
		return schedule.ClassInvalid
	case domain.ErrPermanent:
		return schedule.ClassPermanent
	case domain.ErrTransient:
		return schedule.ClassTransient
	case domain.ErrRateLimit:
		return schedule.ClassCapacity
	default:
		return fallback
	}
}

// classifyHTTPStatus 把 HTTP 状态码归类成 schedule.ErrorClass。
func classifyHTTPStatus(code int) schedule.ErrorClass {
	switch {
	case code >= 200 && code < 300:
		return schedule.ClassSuccess
	case code == 401 || code == 403:
		return schedule.ClassPermanent
	case code == 429:
		return schedule.ClassCapacity
	case code >= 500:
		return schedule.ClassTransient
	case code >= 400:
		return schedule.ClassInvalid
	default:
		return schedule.ClassUnknown
	}
}

// convertDecisions schedule.Decision → domain.Attempt（trace shape）。
//
// Outcome 推导：Success → success；其它 → fallback（除最后一个 → fail）
func convertDecisions(ds []schedule.Decision) []domain.Attempt {
	out := make([]domain.Attempt, len(ds))
	for i, d := range ds {
		outcome := domain.AttemptFallback
		if d.Result.Class == schedule.ClassSuccess {
			outcome = domain.AttemptSuccess
		} else if i == len(ds)-1 {
			outcome = domain.AttemptFail
		}
		out[i] = domain.Attempt{
			Index:      d.AttemptNum,
			EndpointID: strconv.FormatInt(d.EndpointID, 10),
			Outcome:    outcome,
			LatencyMs:  d.Result.Latency.Milliseconds(),
			ErrorClass: d.Result.Class.String(),
		}
	}
	return out
}

func feedErrMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// copyHeaders 把上游响应头拷贝到客户端响应头（除内部使用的 Content-Length，gin 会重算）。
func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if k == "Content-Length" {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
