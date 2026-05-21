package adapters

import (
	"context"
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
