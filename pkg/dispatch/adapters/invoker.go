package adapters

import (
	"context"
	"io"
	"net/http"

	"github.com/zereker/llm-gateway/pkg/dispatch"
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/invoker"
	"github.com/zereker/llm-gateway/pkg/moderation"
	"github.com/zereker/llm-gateway/pkg/protocol"
	"github.com/zereker/llm-gateway/pkg/selector"
)

// InvokerFactoryAdapter 实现 dispatch.InvokerFactory——把 *invoker.Sender 包成
// dispatch port。
//
// **职责**：For(ep, handler, env) → dispatch.Invoker；Invoker.Invoke 调用
// Sender.Send + 把 Outcome 翻成 dispatch.Verdict 包进 Result。
//
// **不做** reserve / Report / TPM charge——v0.6 这些拆给 dispatch.EndpointQuota
// + Dispatcher 内置。invoker 只负责一次纯调用 + forward。
type InvokerFactoryAdapter struct {
	sender *invoker.Sender
}

// NewInvokerFactory 构造一个 InvokerFactoryAdapter。
func NewInvokerFactory(sender *invoker.Sender) *InvokerFactoryAdapter {
	return &InvokerFactoryAdapter{sender: sender}
}

// For 实现 dispatch.InvokerFactory.For；body 从 env.RawBytes 读。
func (f *InvokerFactoryAdapter) For(ep *domain.Endpoint, handler protocol.Handler, env *domain.RequestEnvelope) dispatch.Invoker {
	return &invokerImpl{
		ep:      ep,
		env:     env,
		handler: handler,
		sender:  f.sender,
	}
}

type invokerImpl struct {
	ep      *domain.Endpoint
	env     *domain.RequestEnvelope
	handler protocol.Handler
	sender  *invoker.Sender
}

// Invoke 实现 dispatch.Invoker.Invoke——纯 HTTP 调用 + classify。
func (i *invokerImpl) Invoke(ctx context.Context) (dispatch.Result, error) {
	var body []byte
	if i.env != nil {
		body = i.env.RawBytes
	}
	outcome, _ := i.sender.Send(ctx, i.ep, i.env, body, i.handler)
	v := dispatch.Verdict{
		Stage:    invokerStageToDispatch(outcome.Stage),
		Class:    selectorClassToDispatch(outcome.Class),
		HTTPCode: outcome.HTTPCode,
		Reason:   outcome.Reason,
		Latency:  outcome.Latency,
	}
	return &invokerResult{
		ep:       i.ep,
		verdict:  v,
		response: outcome.Response,
		handler:  outcome.Handler,
		sender:   i.sender,
	}, nil
}

// invokerResult 实现 dispatch.Result——成功路径走 sender.Forward + 顺手 wrap moderator。
type invokerResult struct {
	ep       *domain.Endpoint
	verdict  dispatch.Verdict
	response *http.Response
	handler  protocol.Handler
	sender   *invoker.Sender
	consumed bool
}

func (r *invokerResult) Verdict() dispatch.Verdict  { return r.verdict }
func (r *invokerResult) Endpoint() *domain.Endpoint { return r.ep }

func (r *invokerResult) StreamTo(ctx context.Context, w http.ResponseWriter) dispatch.StreamReport {
	if r.consumed || r.response == nil || r.handler == nil {
		return dispatch.StreamReport{}
	}
	r.consumed = true

	// 传输解码接缝：vendor（如 Bedrock event-stream）把上游分帧解成协议 handler 认识
	// 的字节流，再进 Feed。TransportDecoder 是可选的——返回 nil = 无需解帧（SSE/JSON，
	// 绝大多数）。这样传输层（分帧）跟协议层（shape 翻译）干净分离。
	if dec, ok := r.handler.(protocol.TransportDecoder); ok {
		if decoded := dec.DecodeTransport(r.response); decoded != nil {
			// 用 decoded 替换 body 供 Forward 读；原 body 的 Close 权交给包装，
			// 保证 Forward 的 defer Close 仍关到真实连接。
			orig := r.response.Body
			r.response.Body = readClose{Reader: decoded, closeFn: orig.Close}
			// 传输已从 vendor 分帧（如 application/vnd.amazon.eventstream）解成
			// SSE：上游 Content-Type 不再描述客户端将收到的字节，强制成
			// text/event-stream，否则 SSE 客户端会拒绝按流解析（Forward 直拷上游头）。
			r.response.Header.Set("Content-Type", "text/event-stream")
		}
	}

	stream := moderation.WrapStream(r.handler.NewResponseStream(), ctx)
	fwd := r.sender.Forward(ctx, w, r.ep, r.response, stream)

	return dispatch.StreamReport{
		Usage:  fwd.Usage,
		Err:    fwd.FeedErr,
		TTFTMs: fwd.TTFTMs,
	}
}

func (r *invokerResult) Close() error {
	if r.consumed || r.response == nil {
		return nil
	}
	r.consumed = true
	return r.response.Body.Close()
}

// =============================================================================
// 跨包 Stage / Class 翻译 helpers
// =============================================================================

func invokerStageToDispatch(s invoker.Stage) dispatch.Stage {
	if s == invoker.StagePrepare {
		return dispatch.StagePrepare
	}
	return dispatch.StageInvoke
}

func selectorClassToDispatch(c selector.ErrorClass) dispatch.Class {
	switch c {
	case selector.ClassSuccess:
		return dispatch.ClassSuccess
	case selector.ClassTransient:
		return dispatch.ClassTransient
	case selector.ClassCapacity:
		return dispatch.ClassCapacity
	case selector.ClassPermanent:
		return dispatch.ClassPermanent
	case selector.ClassInvalid:
		return dispatch.ClassInvalid
	default:
		return dispatch.ClassUnknown
	}
}

// 编译期断言。
var (
	_ dispatch.InvokerFactory = (*InvokerFactoryAdapter)(nil)
	_ dispatch.Invoker        = (*invokerImpl)(nil)
	_ dispatch.Result         = (*invokerResult)(nil)
)

// readClose 把一个 io.Reader（解帧后的流）+ 原 body 的 Close 组合成 ReadCloser——
// Forward 读解帧字节，defer Close 仍关真实上游连接。
type readClose struct {
	io.Reader
	closeFn func() error
}

func (rc readClose) Close() error { return rc.closeFn() }
