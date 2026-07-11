package dispatch

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/trace"
)

// Dispatcher coordinates Selector + Invoker + Policy to route a request to
// the right endpoint and execute it.
//
// **Design philosophy**: Dispatcher itself only runs the Action reducer
// loop; the business truth lives in the three Policy implementations. Adding
// a new retry policy / fallback policy doesn't touch Dispatcher — just write
// a new Policy and inject it.
//
// **Lifecycle**: a single instance (startup wiring), concurrency-safe (no
// per-request state; state is newed up per request).
type Dispatcher struct {
	candidates     CandidateSource
	selector       Selector
	invokerFactory InvokerFactory
	quota          EndpointQuota
	cap            AttemptCap
	retry          RetryPolicy
	fallback       FallbackPolicy
	tracer         trace.Tracer // optional; nil → no span (equivalent to SlogTracer NoOp)
}

// New assembles a Dispatcher.
//
// **Required**: CandidateSource / Selector / InvokerFactory / AttemptCap /
// RetryPolicy / FallbackPolicy — missing any one → panic. Fail-fast exposes
// config errors.
//
// **Optional**: EndpointQuota (not passed = NoopQuota, never rejects).
func New(opts ...Option) *Dispatcher {
	d := &Dispatcher{}
	for _, opt := range opts {
		opt(d)
	}
	if d.candidates == nil {
		panic("dispatch.New: WithCandidates required")
	}
	if d.selector == nil {
		panic("dispatch.New: WithSelector required")
	}
	if d.invokerFactory == nil {
		panic("dispatch.New: WithInvokerFactory required")
	}
	if d.cap == nil {
		panic("dispatch.New: WithCap required")
	}
	if d.retry == nil {
		panic("dispatch.New: WithRetry required")
	}
	if d.fallback == nil {
		panic("dispatch.New: WithFallback required")
	}
	if d.quota == nil {
		d.quota = NoopQuota{}
	}
	if d.tracer == nil {
		d.tracer = trace.NewSlogTracer(nil) // NoOp span, zero overhead on the hot path
	}
	return d
}

// Dispatch is the entry point. Framework-free — it only knows about the
// stdlib http.ResponseWriter and the typed Input.
//
// **Flow**:
//
//	state := newState(in, cap.Resolve(in))
//	for {
//	    action := d.step(ctx, w, state)
//	    switch action.(type) { Continue / Switch / Stream / Abort }
//	}
//
// **Returns**: Outcome.Result == OutcomeStreamed means the response has
// already been written out via w; otherwise the middleware needs to write an
// error response based on HTTPCode / Class / Reason. The caller maps
// outcome.Decision / Usage / RoutedModel / Error etc. back onto RC (dispatch
// never touches RC directly).
func (d *Dispatcher) Dispatch(ctx context.Context, w http.ResponseWriter, in Input) Outcome {
	s := newState(in, d.cap.Resolve(in))

	ctx, span := d.tracer.StartSpan(ctx, "dispatch.request")
	span.SetAttribute("dispatch.model", s.CurrentModelName())
	span.SetAttribute("dispatch.group", s.Group())
	span.SetAttribute("dispatch.attempt_cap", s.AttemptsCap())
	defer span.End()

	for {
		switch a := d.step(ctx, w, s).(type) {
		case Continue:
			// pick another endpoint for the same model; Record already
			// excluded it, so go straight into the next round of Select
		case Switch:
			prev := s.CurrentModelName()
			s.SetModel(a.Next)
			d.tracer.Log(ctx, "dispatch.fallback", map[string]string{
				"from": prev, "to": modelName(a.Next),
			})
		case Stream:
			// ApplyStream already ran inside step; Stream{} is only a
			// "handled" signal
			out := s.Outcome()
			span.SetAttribute("dispatch.outcome", out.Result.String())
			span.SetAttribute("dispatch.routed_model", modelName(out.RoutedModel))
			span.SetAttribute("dispatch.attempts", s.Attempts())
			return out
		case Abort:
			s.SetAbort(a)
			out := s.Outcome()
			span.SetAttribute("dispatch.outcome", out.Result.String())
			span.SetAttribute("dispatch.http_code", a.HTTPCode)
			span.SetAttribute("dispatch.attempts", s.Attempts())
			return out
		}
	}
}

// step runs one business iteration: select → quota.Reserve → handler lookup
// → invoke → selector.Report → policy decision; also runs quota.Charge on a
// successful stream.
//
// **Special case**: when the decision is Stream, StreamTo + ApplyStream are
// completed directly inside step (the res resource is deferred-Closed within
// step's stack frame, so it can't be returned across methods); step returns
// Stream{} purely as a signal for Dispatch to exit the loop.
func (d *Dispatcher) step(ctx context.Context, w http.ResponseWriter, s *state) Action {
	// The caller is gone (client disconnect / request deadline): retrying
	// would burn attempts against nobody, and every canceled Do would come
	// back as a Transient verdict that wrongly penalizes healthy endpoints.
	// Bail out before touching any endpoint. (The mid-stream counterpart of
	// this check is isClientAbort below.)
	if ctx.Err() != nil {
		return clientAbort(ctx)
	}

	if s.Exhausted() {
		return Abort{
			Result:   OutcomeNoEndpoint,
			Class:    ClassUnknown,
			HTTPCode: 503,
			Reason:   "attempts exhausted",
		}
	}

	ctx, span := d.tracer.StartSpan(ctx, "dispatch.attempt")
	span.SetAttribute("attempt.model", s.CurrentModelName())
	span.SetAttribute("attempt.index", s.Attempts())
	defer span.End()

	// === CandidateSource → filter → Selector.Pick: three separate steps ===
	candidates, err := d.candidates.ListForModel(ctx, s.CurrentModelName(), s.Group())
	if err != nil {
		return Abort{
			Result:   OutcomeDepFail,
			Class:    ClassTransient,
			HTTPCode: 503,
			Reason:   "candidates: " + err.Error(),
		}
	}
	span.SetAttribute("attempt.candidates", len(candidates))
	eligible := filterEligible(candidates, s.Envelope(), s.Handlers())
	span.SetAttribute("attempt.eligible", len(eligible))
	if len(eligible) == 0 {
		span.SetAttribute("attempt.exit", "no_eligible")
		return d.fallback.OnExhausted(s)
	}
	ep, err := d.selector.Pick(ctx, eligible, s.PickQuery())
	if err != nil {
		return Abort{
			Result:   OutcomeDepFail,
			Class:    ClassTransient,
			HTTPCode: 503,
			Reason:   "select: " + err.Error(),
		}
	}
	if ep == nil {
		// candidates were eligible, but the picker skipped all of them (e.g.
		// due to cooldown) — hand it off to FallbackPolicy
		span.SetAttribute("attempt.exit", "picker_skipped_all")
		return d.fallback.OnExhausted(s)
	}
	// Pick incremented ep's P2C pending-call counter; release it exactly once
	// when this attempt returns, no matter which path (reserve deny / handler
	// miss / invoke error / success / stream break / panic unwind). Decoupled
	// from Report because the success+stream-break path Reports twice.
	defer d.selector.Release(ctx, ep)
	annotateEndpoint(span, ep)

	// === EndpointQuota.Reserve (pre-deduction) ===
	if denied, qerr := d.quota.Reserve(ctx, ep); denied != nil || qerr != nil {
		v := quotaVerdictToAttempt(denied, qerr)
		annotateVerdict(span, v)
		s.Record(ep, v)
		d.selector.Report(ctx, ep, v)
		return d.retry.Decide(s, v)
	}

	// === Handler lookup ===
	// dynamically compose the Handler from (endpoint, srcProto).
	// the eligibility filter already blocks endpoints missing a handler;
	// this is a second line of defense.
	handler := s.Handlers().Get(ep, s.in.SourceProtocol())
	if handler == nil {
		v := Verdict{
			Stage:    StagePrepare,
			Class:    ClassPermanent,
			HTTPCode: 502,
			Reason:   "no handler for endpoint+srcProto",
		}
		annotateVerdict(span, v)
		s.Record(ep, v)
		// The endpoint was reserved but never contacted (no handler to build
		// the call) — refund its RPM/RPS reserve so a config gap doesn't
		// silently throttle the endpoint.
		d.quota.Release(ctx, ep)
		d.selector.Report(ctx, ep, v)
		return d.retry.Decide(s, v)
	}

	// === Invoker.Invoke (pure HTTP) ===
	inv := d.invokerFactory.For(ep, handler, s.Envelope())
	res, ierr := inv.Invoke(ctx)
	if ctx.Err() != nil {
		// The request ctx died while invoking: whatever Invoke returned
		// (usually a canceled Do surfacing as a Transient verdict) was caused
		// by the client going away, not by the endpoint. Record/Report here
		// would push a healthy endpoint into cooldown and pollute its stats —
		// skip both and end the loop. The endpoint quota reserve is kept: the
		// upstream may or may not have been reached, and the sliding window
		// self-heals (same rationale as real invoke failures, docs/04 §10).
		if ierr == nil && res != nil {
			res.Close()
		}
		span.SetAttribute("attempt.exit", "client_abort")
		return clientAbort(ctx)
	}
	if ierr != nil {
		// "unable to construct the call" is very rare (the current default
		// InvokerFactory never hits this; it's a path left open for custom
		// Invoker implementations). Handle it the same as other failure
		// paths: Record + Report + Policy decision — Aborting directly would
		// bypass cooldown/retry and blow up a transient error into a 503.
		v := Verdict{
			Stage:  StageInvoke,
			Class:  ClassTransient,
			Reason: "invoke: " + ierr.Error(),
		}
		annotateVerdict(span, v)
		s.Record(ep, v)
		// The call could not even be constructed — the endpoint was never
		// contacted, so refund its reserve.
		d.quota.Release(ctx, ep)
		d.selector.Report(ctx, ep, v)
		return d.retry.Decide(s, v)
	}
	defer res.Close()

	verdict := res.Verdict()
	annotateVerdict(span, verdict)
	s.Record(ep, verdict)
	d.selector.Report(ctx, ep, verdict)

	action := d.retry.Decide(s, verdict)
	if _, ok := action.(Stream); ok {
		// success path — consume res inside step (its resource lifetime
		// can't cross method boundaries)
		rep := res.StreamTo(ctx, w)
		s.ApplyStream(rep)
		// === EndpointQuota.ChargeUsage (post-deduction, fire-and-forget) ===
		// Only charge TPM for a cleanly completed stream. On a mid-stream break
		// rep.Usage may be partial or an upstream-reported total that was never
		// fully delivered — charging it over-counts the endpoint's TPM bucket.
		if rep.Err == nil {
			d.quota.ChargeUsage(ctx, ep, rep.Usage)
		}
		if rep.Err != nil {
			// stream interrupted after a 200: the status code is already
			// written, so no retry is possible. Whether we penalize the
			// endpoint depends on who broke the connection:
			//   - client actively disconnected (request ctx canceled / EPIPE
			//     while writing to the client) — not the endpoint's fault.
			//     Penalizing it would let "clients frequently canceling"
			//     wrongly push healthy endpoints into cooldown.
			//   - upstream RST / connection dropped mid-stream — the
			//     endpoint's fault, and cooldown/stats must see it, otherwise
			//     a bad endpoint that "cuts off after 200" would show up as
			//     100% success forever while traffic keeps hitting it.
			// the pre-stream Success Report has already been sent; only when
			// the *upstream* broke the connection do we send a supplementary
			// StageStream transient verdict to override it. A client
			// disconnect keeps the success verdict and isn't penalized.
			if isClientAbort(ctx, rep.Err) {
				span.SetAttribute("stream.abort", "client")
			} else {
				sv := Verdict{
					Stage:  StageStream,
					Class:  ClassTransient,
					Reason: "stream: " + rep.Err.Error(),
				}
				span.SetAttribute("stream.err", rep.Err.Error())
				d.selector.Report(ctx, ep, sv)
			}
		}
		return Stream{}
	}
	return action
}

// clientAbort builds the terminal Action for a request whose ctx was
// canceled before response headers were written. This is not an endpoint
// failure: no verdict is recorded or reported, so cooldown and stats stay
// clean. 499 follows the nginx "client closed request" convention; a
// deadline (gateway-enforced timeout) maps to 504 instead.
func clientAbort(ctx context.Context) Abort {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return Abort{
			Result:   OutcomeClientAbort,
			Class:    ClassUnknown,
			HTTPCode: 504,
			Reason:   "request deadline exceeded",
		}
	}

	return Abort{
		Result:   OutcomeClientAbort,
		Class:    ClassUnknown,
		HTTPCode: 499,
		Reason:   "client disconnected before response",
	}
}

// isClientAbort determines whether a stream interruption was the client
// actively disconnecting (as opposed to an upstream RST / mid-stream drop).
//
// The primary signal is the request's ctx.Err() — gin cancels
// c.Request.Context() on client disconnect / request timeout, which also
// covers the race where "w.Write hits EPIPE, then ctx gets canceled"; as a
// fallback it also matches the context error directly (StreamTo internally
// propagates a canceled ctx as rep.Err).
func isClientAbort(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// modelName safely reads the Model field off *domain.ModelService (nil-safe).
func modelName(m *domain.ModelService) string {
	if m == nil {
		return ""
	}
	return m.Model
}

// annotateEndpoint tags the current span with attributes once an endpoint
// has been selected.
func annotateEndpoint(span trace.Span, ep *domain.Endpoint) {
	if ep == nil {
		return
	}
	span.SetAttribute("endpoint.id", strconv.FormatInt(ep.ID, 10))
	span.SetAttribute("endpoint.vendor", ep.Vendor)
	span.SetAttribute("endpoint.protocol", ep.Protocol.String())
}

// annotateVerdict tags the span with the attempt's result.
func annotateVerdict(span trace.Span, v Verdict) {
	span.SetAttribute("verdict.stage", v.Stage.String())
	span.SetAttribute("verdict.class", v.Class.String())
	if v.HTTPCode != 0 {
		span.SetAttribute("verdict.http_code", v.HTTPCode)
	}
	if v.Reason != "" {
		span.SetAttribute("verdict.reason", v.Reason)
	}
}

// quotaVerdictToAttempt translates EndpointQuota.Reserve's rejection
// (QuotaVerdict) into a dispatch.Verdict (an attempt-level report used for
// retry / Selector.Report).
//
// **Class semantics** (docs/04 §8):
//   - ClassCapacity ── a genuine quota rejection: retry with a different
//     endpoint + write a capacity cooldown for this endpoint
//   - ClassUnknown  ── a dependency failure (e.g. Redis error): retry with a
//     different endpoint, but **do not write a cooldown** — we must not
//     mislabel "Redis is flaky" as "the endpoint is broken", or a single
//     blip would push every healthy endpoint on the path into cooldown,
//     leaving a lingering TTL of contamination even after recovery
//
// denied.Class must be filled explicitly by the EndpointQuota implementation;
// denied == nil but qerr != nil (the implementation returned an error
// directly) is likewise treated as a dependency failure (Unknown).
func quotaVerdictToAttempt(denied *QuotaVerdict, qerr error) Verdict {
	if denied != nil {
		reason := denied.Reason
		if denied.BucketKey != "" && reason == "" {
			reason = "endpoint quota exhausted: " + denied.BucketKey
		}
		return Verdict{Stage: StageReserve, Class: denied.Class, Reason: reason}
	}
	return Verdict{
		Stage:  StageReserve,
		Class:  ClassUnknown,
		Reason: "endpoint quota (store error): " + qerr.Error(),
	}
}
