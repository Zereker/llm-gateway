package adapters

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/zereker/llm-gateway/internal/dispatch"
	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/invoker"
	"github.com/zereker/llm-gateway/internal/moderation"
	"github.com/zereker/llm-gateway/internal/policy"
	"github.com/zereker/llm-gateway/internal/protocol"
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
		Stage:      invokerStageToDispatch(outcome.Stage),
		Class:      invokerClassToDispatch(outcome.Class),
		HTTPCode:   outcome.HTTPCode,
		Reason:     outcome.Reason,
		Latency:    outcome.Latency,
		RetryAfter: outcome.RetryAfter,
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

	// Transport decoding seam: a vendor (e.g. Bedrock event-stream) decodes the
	// upstream's framing into the byte stream the protocol handler
	// understands, which then goes into Feed. TransportDecoder is optional —
	// returning nil means no deframing is needed (SSE/JSON, the vast
	// majority). This keeps the transport layer (framing) cleanly separated
	// from the protocol layer (shape translation).
	if dec, ok := r.handler.(protocol.TransportDecoder); ok {
		if decoded := dec.DecodeTransport(r.response); decoded != nil {
			// Replace body with decoded for Forward to read; hand the
			// original body's Close over to the wrapper, so Forward's
			// deferred Close still closes the real connection.
			orig := r.response.Body
			r.response.Body = readClose{Reader: decoded, closeFn: orig.Close}
			// The transport has been deframed from the vendor's framing
			// (e.g. application/vnd.amazon.eventstream) into SSE: the
			// upstream Content-Type no longer describes the bytes the client
			// will receive, so force it to text/event-stream — otherwise SSE
			// clients will refuse to parse it as a stream (Forward copies
			// upstream headers directly).
			r.response.Header.Set("Content-Type", "text/event-stream")
		}
	}

	stream := moderation.WrapStream(ctx, r.handler.NewResponseStream())
	fwd := r.sender.Forward(ctx, w, r.ep, r.response, stream)

	return forwardResultToStreamReport(fwd)
}

func forwardResultToStreamReport(fwd invoker.ForwardResult) dispatch.StreamReport {
	report := dispatch.StreamReport{
		Usage:        fwd.Usage,
		Err:          fwd.FeedErr,
		TTFTMs:       fwd.TTFTMs,
		Committed:    fwd.Committed,
		LocalFailure: errors.Is(fwd.FeedErr, moderation.ErrPolicyEnforcement),
	}
	if fwd.FeedErr != nil && !fwd.Committed {
		reason := "response stream processing failed"
		if report.LocalFailure {
			reason = "response policy enforcement failed"
		}

		report.Prewrite = &dispatch.Verdict{Stage: dispatch.StageStream, Class: dispatch.ClassTransient, HTTPCode: 503, Reason: reason}
		if errors.Is(fwd.FeedErr, policy.ErrDenied) {
			report.Prewrite.Class = dispatch.ClassInvalid
			report.Prewrite.HTTPCode = 400
			report.Prewrite.Reason = "content rejected by response policy"
		}
	}

	return report
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

func invokerClassToDispatch(c invoker.Class) dispatch.Class {
	switch c {
	case invoker.ClassSuccess:
		return dispatch.ClassSuccess
	case invoker.ClassTransient:
		return dispatch.ClassTransient
	case invoker.ClassCapacity:
		return dispatch.ClassCapacity
	case invoker.ClassPermanent:
		return dispatch.ClassPermanent
	case invoker.ClassInvalid:
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

// readClose combines an io.Reader (the deframed stream) with the original
// body's Close into a ReadCloser — Forward reads the deframed bytes, and the
// deferred Close still closes the real upstream connection.
type readClose struct {
	io.Reader
	closeFn func() error
}

func (rc readClose) Close() error { return rc.closeFn() }
