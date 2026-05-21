package invoker

import (
	"context"
	"net/http"
	"time"

	"github.com/zereker/llm-gateway/pkg/dispatch"
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/moderation"
	"github.com/zereker/llm-gateway/pkg/protocol"
	"github.com/zereker/llm-gateway/pkg/selector"
)

// DispatchInvokerFactory 实现 dispatch.InvokerFactory——把 *Sender 包成
// dispatch port。
//
// **职责**：For(ep, env, body, handler) → dispatch.Invoker。Invoker.Invoke
// 调用 Sender.Send + 把 Outcome 翻成 dispatch.Verdict 包进 Result。
//
// **不做** reserve / Report / TPM charge——这些已经在 v0.6 拆给 dispatch.EndpointQuota
// + dispatcher 内置（Selector.Report）。invoker 只负责一次纯调用 + forward。
type DispatchInvokerFactory struct {
	sender *Sender
}

// NewDispatchInvokerFactory 构造一个 DispatchInvokerFactory。
func NewDispatchInvokerFactory(sender *Sender) *DispatchInvokerFactory {
	return &DispatchInvokerFactory{sender: sender}
}

// For 实现 dispatch.InvokerFactory.For。
func (f *DispatchInvokerFactory) For(ep *domain.Endpoint, env *domain.RequestEnvelope, body []byte, handler protocol.Handler) dispatch.Invoker {
	return &dispatchInvoker{
		ep:      ep,
		env:     env,
		body:    body,
		handler: handler,
		sender:  f.sender,
	}
}

type dispatchInvoker struct {
	ep      *domain.Endpoint
	env     *domain.RequestEnvelope
	body    []byte
	handler protocol.Handler
	sender  *Sender
}

// Invoke 实现 dispatch.Invoker.Invoke——纯 HTTP 调用 + classify。
func (i *dispatchInvoker) Invoke(ctx context.Context) (dispatch.Result, error) {
	outcome, _ := i.sender.Send(ctx, i.ep, i.env, i.body, i.handler)
	v := dispatch.Verdict{
		Stage:    invokerStageToDispatch(outcome.Stage),
		Class:    selectorClassToDispatch(outcome.Class),
		HTTPCode: outcome.HTTPCode,
		Reason:   outcome.Reason,
		Latency:  outcome.Latency,
	}
	return &dispatchResult{
		ep:       i.ep,
		verdict:  v,
		response: outcome.Response,
		handler:  outcome.Handler,
		sender:   i.sender,
	}, nil
}

// dispatchResult 实现 dispatch.Result——成功路径走 sender.Forward + 顺手 wrap moderator。
//
// StreamTo / Close 二选一调用一次；StreamTo 之后 Close 是 no-op。
type dispatchResult struct {
	ep       *domain.Endpoint
	verdict  dispatch.Verdict
	response *http.Response
	handler  protocol.Handler
	sender   *Sender
	consumed bool
}

func (r *dispatchResult) Verdict() dispatch.Verdict  { return r.verdict }
func (r *dispatchResult) Endpoint() *domain.Endpoint { return r.ep }

func (r *dispatchResult) StreamTo(ctx context.Context, w http.ResponseWriter) dispatch.StreamReport {
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

func (r *dispatchResult) Close() error {
	if r.consumed || r.response == nil {
		return nil
	}
	r.consumed = true
	return r.response.Body.Close()
}

// =============================================================================
// helpers — 跨包 Stage / Class 翻译
// =============================================================================

func invokerStageToDispatch(s Stage) dispatch.Stage {
	if s == StagePrepare {
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

// _ ensures Forward latency timer reference still compiles when unused; placeholder.
var _ = time.Time{}

// 编译期断言。
var (
	_ dispatch.InvokerFactory = (*DispatchInvokerFactory)(nil)
	_ dispatch.Invoker        = (*dispatchInvoker)(nil)
	_ dispatch.Result         = (*dispatchResult)(nil)
)
