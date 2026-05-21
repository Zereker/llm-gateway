package invoker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/metric"
	"github.com/zereker/llm-gateway/pkg/protocol"
	"github.com/zereker/llm-gateway/pkg/selector"
)

// Send 调一次上游，不做 retry / cooldown / 选路。
//
// **v0.6 融合后**：Send 不再自己查 adapter / translator——caller 已经按
// (endpoint, srcProto) 从 rc.Handlers 取出 protocol.Handler 传进来。invoker
// 只负责：PrepareCall（让 handler 做完 pre-call 协议转换 + HTTP 构造）→
// client.Do → classify → 填 Outcome。
//
// 流程：
//  1. handler.PrepareCall(ctx, ep, srcBody) → *protocol.Call（req + upstreamBody）
//  2. fan-out OnUpstreamRequest hook（拿到 upstreamBody）
//  3. client.Do
//  4. 按 HTTP status + handler 的 Classify（如果实现）分类，填 Outcome
//  5. defer fan-out OnAttemptComplete hook（成功 / 失败都触发）
//
// 任意步骤失败 → Outcome.Class != ClassSuccess + Response==nil。
// 成功 → Response.Body 交给 caller 自行 forward + close。
//
// **PrepareCall 失败处理**：通过 errors.As(*protocol.PrepareError) 判定阶段：
//   - PhaseTranslate → ClassInvalid + ErrInvalidRequest（caller 应 abort 400）
//   - PhaseBuild     → ClassPermanent（caller 走 retry 也无意义；新 ep 可能也失败）
func (s *Sender) Send(
	ctx context.Context,
	ep *domain.Endpoint,
	env *domain.RequestEnvelope,
	srcBody []byte,
	handler protocol.Handler,
) (out Outcome, retErr error) {
	start := time.Now()

	defer func() {
		s.hooks.fireComplete(ctx, ep, out)
		emitUpstreamMetrics(ep, out)
	}()

	// ClientRequest fan-out：最早期，无论后续 handler 走不走得通都触发。
	// 这是网关接收到的原始字节——审计 / 合规观察这里就够了。
	s.hooks.fireClientRequest(ctx, ep, srcBody)

	if handler == nil {
		out = Outcome{
			Stage:   StagePrepare,
			Class:   selector.ClassPermanent,
			Reason:  "no handler for endpoint+srcProto",
			Latency: time.Since(start),
		}
		return out, nil
	}

	call, err := handler.PrepareCall(ctx, ep, srcBody)
	if err != nil {
		out, retErr = handlePrepareError(err, start)
		return out, retErr
	}

	// UpstreamRequest fan-out：handler.PrepareCall 已确定上游字节形态；必须在
	// client.Do 之前，让 observer 能在请求真正发出前看到 body（审计 / 备份场景
	// 需要"先记录后发送"）。跨协议下这里跟 ClientRequest 内容不同。
	s.hooks.fireUpstreamRequest(ctx, ep, call.UpstreamBody)

	req := call.Request.WithContext(ctx)
	resp, err := s.client.Do(req)
	if err != nil {
		out = Outcome{
			Class:   selector.ClassTransient,
			Reason:  "upstream call: " + err.Error(),
			Latency: time.Since(start),
		}
		return out, nil
	}

	class := classifyHTTPStatus(resp.StatusCode)
	// 可选 Classifier 接管：handler 自定义看 error body 细化 class。
	// combined Handler 自动透传到底层 adapter.Classifier；vendor 直接实现 Classifier 也 OK。
	if class != selector.ClassSuccess {
		if cls, ok := handler.(protocol.Classifier); ok {
			peeked := peekBodyForClassify(resp)
			if refined := cls.Classify(resp.StatusCode, peeked); refined != nil {
				class = adapterErrToScheduleClass(refined.Class, class)
			}
		}
	}

	if class != selector.ClassSuccess {
		_ = resp.Body.Close()
		out = Outcome{
			Class:    class,
			HTTPCode: resp.StatusCode,
			Reason:   fmt.Sprintf("upstream status %d", resp.StatusCode),
			Latency:  time.Since(start),
		}
		return out, nil
	}

	out = Outcome{
		Response: resp,
		Class:    class,
		HTTPCode: resp.StatusCode,
		Latency:  time.Since(start),
		Handler:  handler, // 给 Forward 阶段拿 ResponseStream 用
	}
	return out, nil
}

// handlePrepareError 把 protocol.PrepareError 翻译成 Outcome + retErr。
// 所有 prepare 阶段失败一律标 Stage=StagePrepare，让 Policy 区分 prepare vs invoke。
func handlePrepareError(err error, start time.Time) (Outcome, error) {
	var pe *protocol.PrepareError
	if errors.As(err, &pe) {
		switch pe.Phase {
		case protocol.PhaseTranslate:
			return Outcome{
				Stage:   StagePrepare,
				Class:   selector.ClassInvalid,
				Reason:  "translate request: " + pe.Err.Error(),
				Latency: time.Since(start),
			}, fmt.Errorf("%w: %v", ErrInvalidRequest, pe.Err)
		case protocol.PhaseBuild:
			return Outcome{
				Stage:   StagePrepare,
				Class:   selector.ClassPermanent,
				Reason:  "build request: " + pe.Err.Error(),
				Latency: time.Since(start),
			}, nil
		}
	}
	return Outcome{
		Stage:   StagePrepare,
		Class:   selector.ClassPermanent,
		Reason:  "prepare: " + err.Error(),
		Latency: time.Since(start),
	}, nil
}

// emitUpstreamMetrics 发 docs/08 §3 的 upstream_requests_total + upstream_duration_seconds。
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
		"protocol", ep.Protocol.String(),
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

// peekBodyForClassify 错误响应时小量读 body（≤4KiB）让 Classifier 解析；
// 读完替换 resp.Body 让后续路径还能读到完整 body。
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
// 不能 1:1 映射：domain.ErrUnknown 兜底到原 fallback class（HTTP-status 推导的那个）。
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
