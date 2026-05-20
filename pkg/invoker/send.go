package invoker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/zereker/llm-gateway/pkg/adapter"
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/metric"
	"github.com/zereker/llm-gateway/pkg/selector"
	"github.com/zereker/llm-gateway/pkg/translator"
)

// Send 调一次上游，不做 retry / cooldown / 选路。
//
// 流程：
//  1. lookup 取 adapter.Factory + translator.Find
//  2. translator.TranslateRequest（同协议 identity 透传）
//  3. factory.NewSession + sess.BuildRequest
//  4. fan-out OnRequest hook（拿到上游 request body）
//  5. client.Do
//  6. 按 HTTP status + Adapter Classifier 分类，填 Outcome
//  7. defer fan-out OnAttemptComplete hook（成功 / 失败都触发）
//
// 任意步骤失败 → Outcome.Class != ClassSuccess + Response==nil（资源已 close）。
// 成功 → Response.Body 交给 caller 自行 forward + close（Sender.Forward 会 defer Close）。
//
// **特殊**：translator.TranslateRequest 失败返 (Outcome{Class:ClassInvalid}, ErrInvalidRequest)，
// caller 应直接 abort 400（不重试）。
func (s *Sender) Send(
	ctx context.Context,
	ep *domain.Endpoint,
	env *domain.RequestEnvelope,
	srcBody []byte,
) (out Outcome, retErr error) {
	start := time.Now()

	// AttemptComplete fan-out 走 defer + named return，覆盖所有 return 路径
	// （含 panic 之外的所有正常分支）。panic 不 fire——跟 hook panic 不 recover 的
	// 设计一致，让 panic 走调用栈到 M9 兜底。
	defer func() {
		s.hooks.fireComplete(ctx, ep, out)
		// docs/08 §3: upstream_requests_total / upstream_duration_seconds
		emitUpstreamMetrics(ep, out)
	}()

	// ClientRequest fan-out：最早期，无论后续 factory / translator 走不走得通都触发。
	// 这是网关接收到的原始字节——审计 / 合规观察这里就够了。
	s.hooks.fireClientRequest(ctx, ep, srcBody)

	factory := s.lookup.Get(ep.Vendor)
	if factory == nil {
		out = Outcome{
			Class:   selector.ClassPermanent,
			Reason:  "no adapter registered for vendor: " + ep.Vendor,
			Latency: time.Since(start),
		}
		return out, nil
	}

	tgtProto := factory.Metadata().NativeProtocol
	srcProto := env.SourceProtocol
	trans := translator.Find(srcProto, tgtProto)
	if trans == nil {
		out = Outcome{
			Class:   selector.ClassPermanent,
			Reason:  fmt.Sprintf("no translator for %s → %s", srcProto, tgtProto),
			Latency: time.Since(start),
		}
		return out, nil
	}

	upstreamBody, err := trans.TranslateRequest(srcBody)
	if err != nil {
		out = Outcome{
			Class:   selector.ClassInvalid,
			Reason:  "translate request: " + err.Error(),
			Latency: time.Since(start),
		}
		return out, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	sess, err := factory.NewSession(ctx, ep, env)
	if err != nil {
		out = Outcome{
			Class:   selector.ClassTransient,
			Reason:  "NewSession: " + err.Error(),
			Latency: time.Since(start),
		}
		return out, nil
	}

	req, err := sess.BuildRequest(upstreamBody)
	if err != nil {
		_ = sess.Close()
		out = Outcome{
			Class:   selector.ClassPermanent,
			Reason:  "BuildRequest: " + err.Error(),
			Latency: time.Since(start),
		}
		return out, nil
	}

	// UpstreamRequest fan-out：BuildRequest 已确定上游字节形态；必须在 client.Do
	// 之前，让 observer 能在请求真正发出前看到 body（审计 / 备份场景需要"先记录
	// 后发送"）。跨协议 translator 下这里跟 ClientRequest 内容不同。
	s.hooks.fireUpstreamRequest(ctx, ep, upstreamBody)

	req = req.WithContext(ctx)
	resp, err := s.client.Do(req)
	if err != nil {
		_ = sess.Close()
		out = Outcome{
			Class:   selector.ClassTransient,
			Reason:  "upstream call: " + err.Error(),
			Latency: time.Since(start),
		}
		return out, nil
	}

	class := classifyHTTPStatus(resp.StatusCode)
	// 可选 Adapter Classifier 接管：vendor 自己看 error body 细化 class。
	// 例：OpenAI 区分 insufficient_quota（permanent）vs 真 rate-limit（capacity）；
	// Anthropic 529 overloaded_error → capacity。
	if class != selector.ClassSuccess {
		if cls, ok := factory.(adapter.Classifier); ok {
			peeked := peekBodyForClassify(resp)
			if refined := cls.Classify(resp.StatusCode, peeked); refined != nil {
				class = adapterErrToScheduleClass(refined.Class, class)
			}
		}
	}

	if class != selector.ClassSuccess {
		_ = resp.Body.Close()
		_ = sess.Close()
		out = Outcome{
			Class:    class,
			HTTPCode: resp.StatusCode,
			Reason:   fmt.Sprintf("upstream status %d", resp.StatusCode),
			Latency:  time.Since(start),
		}
		return out, nil
	}

	// 成功：Response 给 caller；session 立即 close（v0.5 slim Session 无流式状态）
	_ = sess.Close()
	out = Outcome{
		Response:   resp,
		Class:      class,
		HTTPCode:   resp.StatusCode,
		Latency:    time.Since(start),
		Translator: trans,
	}
	return out, nil
}

// emitUpstreamMetrics 发 docs/08 §3 的 upstream_requests_total + upstream_duration_seconds。
//
// 调用时机：Send defer 收尾时（成功 / 失败都触发）；标签维度按 docs §3 配置。
func emitUpstreamMetrics(ep *domain.Endpoint, out Outcome) {
	if ep == nil {
		return
	}
	vendor := ep.Vendor
	endpointID := strconv.FormatInt(ep.ID, 10)
	model := ep.Model
	result := "ok"
	errClass := ""
	if out.Class != selector.ClassSuccess {
		result = "error"
		errClass = out.Class.String()
	}
	metric.Inc(metric.InvokerRequestsTotal,
		"vendor", vendor,
		"endpoint_id", endpointID,
		"model", model,
		"native_protocol", "", // 暂留空；adapter.Factory.Metadata().NativeProtocol 可填
		"result", result,
		"error_class", errClass,
	)
	metric.Observe(metric.InvokerDurationSeconds, out.Latency.Seconds(),
		"vendor", vendor,
		"endpoint_id", endpointID,
		"model", model,
		"result", result,
		"error_class", errClass,
	)
}

// peekBodyForClassify 错误响应时小量读 body（≤4KiB）让 adapter Classifier 解析；
// 读完替换 resp.Body 让后续路径还能读到完整 body。
//
// 错误 body 通常都很小（OpenAI/Anthropic 都是几百字节 JSON）；4KiB 上限保护
// 异常巨大的 body 不会爆内存。
//
// 读取失败（已被 reader 消费 / 超时）：返回 nil，让 Classifier fallback 到 status-only。
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
	resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(peeked), resp.Body))
	return peeked
}

// classifyHTTPStatus 把 HTTP 状态码归类成 selector.ErrorClass。
func classifyHTTPStatus(code int) selector.ErrorClass {
	switch {
	case code >= 200 && code < 300:
		return selector.ClassSuccess
	case code == 401 || code == 403:
		return selector.ClassPermanent
	case code == 429:
		return selector.ClassCapacity
	case code >= 500:
		return selector.ClassTransient
	case code >= 400:
		return selector.ClassInvalid
	default:
		return selector.ClassUnknown
	}
}

// adapterErrToScheduleClass domain.ErrorClass → selector.ErrorClass。
//
// 不能 1:1 映射：domain.ErrUnknown 兜底到原 fallback class（HTTP-status 推导的那个），
// 因为 selector.ClassUnknown 在 IsRetryable 上是 true（会被 retry），而 ErrUnknown 在
// adapter 看应该是"我不知道"——保留原 HTTP-status 分类更安全。
func adapterErrToScheduleClass(c domain.ErrorClass, fallback selector.ErrorClass) selector.ErrorClass {
	switch c {
	case domain.ErrInvalid:
		return selector.ClassInvalid
	case domain.ErrPermanent:
		return selector.ClassPermanent
	case domain.ErrTransient:
		return selector.ClassTransient
	case domain.ErrRateLimit:
		return selector.ClassCapacity
	default:
		return fallback
	}
}
