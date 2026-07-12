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

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/metric"
	"github.com/zereker/llm-gateway/internal/protocol"
)

// Send makes a single call to an upstream; it does not do retry / cooldown /
// routing.
//
// **After the v0.6 merge**: Send no longer looks up adapter / translator
// itself — the caller has already fetched a protocol.Handler from
// rc.Handlers keyed by (endpoint, srcProto) and passes it in. invoker is
// only responsible for: PrepareCall (letting the handler finish pre-call
// protocol conversion + HTTP construction) → client.Do → classify →
// populate Outcome.
//
// Flow:
//  1. handler.PrepareCall(ctx, ep, srcBody) → *protocol.Call (req + upstreamBody)
//  2. fan out the OnUpstreamRequest hook (with upstreamBody)
//  3. client.Do
//  4. classify by HTTP status + the handler's Classify (if implemented), populate Outcome
//  5. defer fan out the OnAttemptComplete hook (fires on both success and failure)
//
// Any step failing → Outcome.Class != ClassSuccess and Response==nil.
// Success → Response.Body is handed to the caller to forward + close itself.
//
// **PrepareCall failure handling**: the phase is determined via
// errors.As(*protocol.PrepareError):
//   - PhaseTranslate → ClassInvalid + ErrInvalidRequest (caller should abort with 400)
//   - PhaseBuild     → ClassPermanent (retrying is pointless; a new endpoint may fail too)
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

	// ClientRequest fan-out: as early as possible, fires regardless of
	// whether the handler downstream succeeds. This is the raw bytes the
	// gateway received — sufficient for audit / compliance observation.
	s.hooks.fireClientRequest(ctx, ep, srcBody)

	if handler == nil {
		out = Outcome{
			Stage:   StagePrepare,
			Class:   ClassPermanent,
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

	// UpstreamRequest fan-out: handler.PrepareCall has already determined
	// the upstream byte shape; this must happen before client.Do so the
	// observer sees the body before the request is actually sent out (audit
	// / backup scenarios need "record before send"). Across protocols this
	// differs in content from ClientRequest.
	s.hooks.fireUpstreamRequest(ctx, ep, call.UpstreamBody)

	req := call.Request.WithContext(ctx)

	resp, err := s.client.Do(req)
	if err != nil {
		out = Outcome{
			Class:   ClassTransient,
			Reason:  "upstream call: " + err.Error(),
			Latency: time.Since(start),
		}

		return out, nil
	}

	class := classifyHTTPStatus(resp.StatusCode)
	// Optional Classifier takeover: the handler can inspect the error body
	// to refine the class. A combined Handler automatically forwards this to
	// the underlying protocol.Classifier; a vendor implementing Classifier
	// directly also works.
	if class != ClassSuccess {
		if cls, ok := handler.(protocol.Classifier); ok {
			peeked := peekBodyForClassify(resp)
			if refined := cls.Classify(resp.StatusCode, peeked); refined != nil {
				class = adapterErrToScheduleClass(refined.Class, class)
			}
		}
	}

	if class != ClassSuccess {
		_ = resp.Body.Close()
		out = Outcome{
			Class:    class,
			HTTPCode: resp.StatusCode,
			Reason:   fmt.Sprintf("upstream status %d", resp.StatusCode),
			Latency:  time.Since(start),
			// reset-aware cooldown input: the upstream's own recovery hint
			// (Retry-After / rate-limit reset headers) overrides the static
			// per-class TTL downstream. Parsed for every failure class — the
			// scheduler decides whether the class cools down at all.
			RetryAfter: parseRetryAfter(resp.Header, time.Now()),
		}

		return out, nil
	}

	out = Outcome{
		Response: resp,
		Class:    class,
		HTTPCode: resp.StatusCode,
		Latency:  time.Since(start),
		Handler:  handler, // used by the Forward stage to obtain a ResponseStream
	}

	return out, nil
}

// handlePrepareError translates a protocol.PrepareError into Outcome + retErr.
// All prepare-stage failures are uniformly tagged Stage=StagePrepare so
// Policy can distinguish prepare failures from invoke failures.
func handlePrepareError(err error, start time.Time) (Outcome, error) {
	var pe *protocol.PrepareError
	if errors.As(err, &pe) {
		switch pe.Phase {
		case protocol.PhaseTranslate:
			return Outcome{
				Stage:   StagePrepare,
				Class:   ClassInvalid,
				Reason:  "translate request: " + pe.Err.Error(),
				Latency: time.Since(start),
			}, fmt.Errorf("%w: %w", ErrInvalidRequest, pe.Err)
		case protocol.PhaseBuild:
			return Outcome{
				Stage:   StagePrepare,
				Class:   ClassPermanent,
				Reason:  "build request: " + pe.Err.Error(),
				Latency: time.Since(start),
			}, nil
		}
	}

	return Outcome{
		Stage:   StagePrepare,
		Class:   ClassPermanent,
		Reason:  "prepare: " + err.Error(),
		Latency: time.Since(start),
	}, nil
}

// emitUpstreamMetrics emits the upstream_requests_total + upstream_duration_seconds metrics from docs/08 §3.
func emitUpstreamMetrics(ep *domain.Endpoint, out Outcome) {
	if ep == nil {
		return
	}

	vendor := ep.Vendor
	endpointID := strconv.FormatInt(ep.ID, 10)
	model := ep.Model
	result := "ok"

	errClass := ""
	if out.Class != ClassSuccess {
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

// peekBodyForClassify reads a small amount of the body (<=4KiB) on error
// responses so Classifier can parse it; afterward it replaces resp.Body so
// downstream code can still read the full body.
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

// classifyHTTPStatus maps an HTTP status code to a Class.
func classifyHTTPStatus(code int) Class {
	switch {
	case code >= 200 && code < 300:
		return ClassSuccess
	case code == 401 || code == 403:
		return ClassPermanent
	case code == 429:
		return ClassCapacity
	case code >= 500:
		return ClassTransient
	case code >= 400:
		return ClassInvalid
	default:
		return ClassUnknown
	}
}

// adapterErrToScheduleClass maps domain.ErrorClass → Class.
//
// Not a 1:1 mapping: domain.ErrUnknown falls back to the original fallback
// class (the one derived from HTTP status).
func adapterErrToScheduleClass(c domain.ErrorClass, fallback Class) Class {
	switch c {
	case domain.ErrInvalid:
		return ClassInvalid
	case domain.ErrPermanent:
		return ClassPermanent
	case domain.ErrTransient:
		return ClassTransient
	case domain.ErrRateLimit:
		return ClassCapacity
	default:
		return fallback
	}
}
