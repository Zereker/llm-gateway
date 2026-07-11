package dispatch

import (
	"context"
	"net/http"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/protocol"
)

// =============================================================================
// Selector port — which endpoint to pick
// =============================================================================

// Selector goes from "a known candidate set → pick one" — a pure picker; it
// doesn't fetch candidates and doesn't do eligibility filtering (those two
// steps are handled by Dispatcher's internal CandidateSource + filterEligible).
//
// **Pick's return contract**:
//
//	(ep, nil)     ── picked one (ep is guaranteed to be in the eligible input
//	                  set, with query.Exclude already excluded)
//	(nil, nil)    ── candidates exhausted (let FallbackPolicy decide whether
//	                  to switch models or abort)
//	(nil, err)    ── dependency failure (e.g. Redis cooldown read failed);
//	                  the driver aborts with 503 directly
//
// **Report**: after each invoke/reserve produces a verdict, Dispatcher calls
// this to feed it back to the Selector's internal cooldown state machine. It
// may fire more than once per attempt (e.g. a supplementary StageStream
// verdict after a success), so it must be idempotent w.r.t. any per-attempt
// accounting.
//
// **Release**: Dispatcher calls this exactly once per Pick that returned a
// non-nil endpoint (via defer), signalling the attempt is done. It backs the
// P2C picker's pending-call counter — decoupled from Report precisely because
// Report can fire twice.
type Selector interface {
	Pick(ctx context.Context, eligible []*domain.Endpoint, q PickQuery) (*domain.Endpoint, error)
	Report(ctx context.Context, ep *domain.Endpoint, v Verdict)
	Release(ctx context.Context, ep *domain.Endpoint)
}

// PickQuery is Selector.Pick's input — only the information the picker
// needs (no Envelope / Identity / Handlers, which have already been
// consumed by CandidateSource and filterEligible).
type PickQuery struct {
	Model      string             // the current round's model (metric label / cooldown key)
	Group      string             // endpoint pool group (used for filtering)
	SessionKey string             // session-affinity key (client's X-Gateway-Session header); empty = no sticky routing
	Exclude    map[int64]struct{} // endpoint IDs already tried in this request
}

// =============================================================================
// CandidateSource port — fetch candidate endpoints by (model, group)
// =============================================================================

// CandidateSource is the port for fetching candidate endpoints by
// (model, group).
//
// **Relationship to Selector**: CandidateSource is responsible for "where
// endpoints come from" (DB or cache, either is fine); Selector is
// responsible for "picking one from a known candidate set". Dispatcher
// chains them together:
//
//	candidates := CandidateSource.ListForModel(ctx, model, group)
//	eligible   := dispatch.filterEligible(candidates, env, handlers)  // internal helper
//	ep         := Selector.Pick(ctx, eligible, query)
type CandidateSource interface {
	ListForModel(ctx context.Context, model, group string) ([]*domain.Endpoint, error)
}

// =============================================================================
// Invoker port — make one downstream call
// =============================================================================

// InvokerFactory builds an Invoker ready to execute from
// (endpoint, handler, envelope).
//
// **A convention, not an interface**: different implementations can have
// completely different For signatures (HTTPFactory.For / BatchFactory.For /
// MockFactory.Pin, etc). Dispatcher just needs a concrete factory — the
// wiring point (cmd/gateway) decides which implementation to use.
//
// **handler parameter**: the request-level end-to-end protocol handler
// (already looked up by the dispatcher from state.Handlers().Get(ep,
// srcProto) based on ep + srcProto).
//
// **Where the body comes from**: env.RawBytes (there's no separate body
// parameter anymore; the invoker reads env internally).
//
// A minimal interface is given here so Dispatcher unit tests can swap in a
// fake implementation.
type InvokerFactory interface {
	For(ep *domain.Endpoint, handler protocol.Handler, env *domain.RequestEnvelope) Invoker
}

// Invoker is one already-configured downstream call. Executed with no
// arguments.
//
// **In scope**: HTTP do, classify, handler.PrepareCall.
//
// **Out of scope**: endpoint quota reserve (owned separately by
// EndpointQuota), Selector.Report (called by Dispatcher after Invoke
// returns). This is the result of v0.6 splitting "reserve → send → report"
// into 3 separate ports.
//
// **Return contract**: err is non-nil only when "the call couldn't be
// constructed" (very rare, e.g. a nil endpoint); upstream errors are
// classified via Result.Verdict().Class.
type Invoker interface {
	Invoke(ctx context.Context) (Result, error)
}

// =============================================================================
// EndpointQuota port — endpoint-level ratelimit pre-deduction + post-deduction
// =============================================================================

// QuotaVerdict is EndpointQuota.Reserve's rejection result — narrower than
// Verdict, describing only "why it was rejected". Once Dispatcher has a
// QuotaVerdict, it translates it into a Verdict and runs it through the
// retry / Report flow.
//
// **Class must be filled explicitly** (docs/04 §8):
//   - ClassCapacity ── a genuine quota rejection: switch endpoints + write a
//     capacity cooldown for this endpoint
//   - ClassUnknown  ── a dependency failure (e.g. Redis error): switch
//     endpoints but **do not write a cooldown**, to avoid mislabeling store
//     flakiness as a bad endpoint
type QuotaVerdict struct {
	Class     Class
	BucketKey string // which bucket rejected it (rl:endpoint:<id>:rpm, etc.); empty = dependency failure
	Reason    string
}

// EndpointQuota is the "pre-deduction + post-deduction" port for
// endpoint-level quota — the dispatcher Reserves (an RPM/RPS hold) before
// calling the invoker, and ChargeUsage (actual token usage) after a
// successful stream.
//
// **Difference from user-level quota**: user-level quota is enforced in the
// M6 middleware (Limit); EndpointQuota is a hard constraint on the endpoint
// side (the vendor's own limits / self-hosted fleet capacity protection),
// enforced by the dispatcher at attempt granularity.
type EndpointQuota interface {
	// Reserve attempts to hold one attempt's worth of quota for ep.
	//   returns (nil, nil)            ── no quota configured, or the reserve succeeded
	//   returns (*QuotaVerdict, nil)  ── quota rejected; Dispatcher translates it into a Verdict for retry
	//   returns (_, err)              ── dependency failure (e.g. Redis error); treated as a rejection too
	Reserve(ctx context.Context, ep *domain.Endpoint) (denied *QuotaVerdict, err error)

	// ChargeUsage writes real token usage to the TPM bucket after a
	// successful stream (fire-and-forget). No-op if usage or ep is nil;
	// charge failures only get logged as a metric and never block the
	// response.
	ChargeUsage(ctx context.Context, ep *domain.Endpoint, usage *domain.Usage)

	// Release rolls back a prior successful Reserve for ep. The dispatcher
	// calls it only when an attempt failed *before the endpoint was ever
	// contacted* (handler-lookup / call-construction failure) — a genuine
	// upstream response (even 429/5xx) keeps the reservation, since we did
	// send the endpoint a request and self-throttling on its rejection is
	// intended. No-op when no quota is configured.
	Release(ctx context.Context, ep *domain.Endpoint)
}

// NoopQuota never rejects and never charges — for deployments with no
// ratelimit configured.
type NoopQuota struct{}

func (NoopQuota) Reserve(_ context.Context, _ *domain.Endpoint) (*QuotaVerdict, error) {
	return nil, nil
}
func (NoopQuota) ChargeUsage(_ context.Context, _ *domain.Endpoint, _ *domain.Usage) {}
func (NoopQuota) Release(_ context.Context, _ *domain.Endpoint)                      {}

// Result is the handle produced by Invoke.
//
// **Lifecycle**: exactly one of StreamTo / Close must be called.
//   - StreamTo: callable only when Verdict.Class == Success; once it starts
//     writing to w, there's no rolling back.
//   - Close: releases the unconsumed body + session; callable at any time;
//     calling it after StreamTo is a no-op.
//
// **Recommended usage**: the driver should `defer res.Close()` immediately
// after getting a Result, which becomes a no-op fallback after StreamTo.
type Result interface {
	Verdict() Verdict
	Endpoint() *domain.Endpoint
	StreamTo(ctx context.Context, w http.ResponseWriter) StreamReport
	Close() error
}

// StreamReport is Result.StreamTo's return value.
//
// **Failure semantics**: once streaming has started (headers already
// written), no error can roll back the status code; Err is only for
// logging / metrics / writing to rc.Error — the client sees a truncated
// stream.
type StreamReport struct {
	Usage  *domain.Usage
	Err    error
	TTFTMs int64
}
