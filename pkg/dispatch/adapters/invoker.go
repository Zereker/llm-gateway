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

// InvokerFactoryAdapter implements dispatch.InvokerFactory — wraps
// *invoker.Sender as a dispatch port.
//
// **Responsibility**: For(ep, handler, env) → dispatch.Invoker; Invoker.Invoke
// calls Sender.Send and translates the Outcome into a dispatch.Verdict
// wrapped in a Result.
//
// **Not handled here**: reserve / Report / TPM charge — as of v0.6 those are
// split out to dispatch.EndpointQuota plus built into Dispatcher. invoker is
// only responsible for a single plain call plus forwarding.
type InvokerFactoryAdapter struct {
	sender *invoker.Sender
}

// NewInvokerFactory constructs an InvokerFactoryAdapter.
func NewInvokerFactory(sender *invoker.Sender) *InvokerFactoryAdapter {
	return &InvokerFactoryAdapter{sender: sender}
}

// For implements dispatch.InvokerFactory.For; the body is read from env.RawBytes.
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

// Invoke implements dispatch.Invoker.Invoke — a plain HTTP call plus classification.
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

// invokerResult implements dispatch.Result — the success path goes through
// sender.Forward and wraps the moderator along the way.
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
// Cross-package Stage / Class translation helpers
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

// Compile-time assertions.
var (
	_ dispatch.InvokerFactory = (*InvokerFactoryAdapter)(nil)
	_ dispatch.Invoker        = (*invokerImpl)(nil)
	_ dispatch.Result         = (*invokerResult)(nil)
)
