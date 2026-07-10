// Package selector provides endpoint selection primitives — filter chain + scorer + picker.
//
// **Design philosophy** (docs/architecture/03-endpoint-scheduling.md §4):
//
//   - This package is **pure selection primitives**: run filter / scorer / picker
//     over a batch of candidates to pick 1 endpoint, with no per-request state
//   - It holds no repo; it knows nothing about protocol / handler / fallback / attempts
//   - Cross-model fallback, attempts / excluded / decisions state, and retry / abort
//     decisions all live in `pkg/dispatch.Dispatcher` — selector only ever sees one batch of candidates
//
// **Call relationship** (since v0.6 dispatch owns scheduling sequencing; selector is reduced to the primitives layer):
//
//	pkg/dispatch.Dispatcher.step (owner of scheduling sequencing)
//	    │  candidates = CandidateSource.ListForModel(ctx, model, group)
//	    │  eligible   = filterEligible(candidates, env, handlers)  // dispatch-internal helper
//	    │  ep         = Selector.Pick(ctx, eligible, query)  ──→  selector.Scheduler.Pick
//	    │  ... Invoker.Invoke / Quota.Reserve / RetryPolicy.Decide ...
//	    │  Selector.Report(ctx, ep, verdict)              ──→    selector.Scheduler.Report
//	    │
//	    └─ adapter: pkg/dispatch/adapters/SelectorAdapter
//	       bridges selector.Scheduler into dispatch.Selector (Pick takes eligible + PickQuery)
//
// **Must not appear in this package**: repo dependencies, http.Request, protocol.Handler, fallback model switching, etc.
//
// See docs/architecture/03-endpoint-scheduling.md §4 + docs/architecture/03a-schedule-overview.md §0-§2 for details.
package selector

import (
	"context"
	"time"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// Candidate is a single endpoint candidate plus its effective weight.
//
// EffectiveWeight is the weight after Runtime Scoring (docs/03 §8) adjustment —
// when scoring isn't enabled, the dispatcher simply fills it with Endpoint.Weight.
type Candidate struct {
	Endpoint        *domain.Endpoint
	EffectiveWeight float64
}

// Request is the input for a single Pick call.
//
// Per docs/03 §4: Request only carries a batch of candidates — it does **not**
// include LoadFallback / FallbackModels / attempts state.
type Request struct {
	Model      string             // current model (before routing = requested model; during fallback routing = fallback model)
	Group      string             // routing group (rc.Identity.Group)
	SessionKey string             // session affinity key (client's X-Gateway-Session header); empty = no sticky session
	Candidates []Candidate        // candidates after eligibility filtering (with EffectiveWeight)
	ExcludeIDs map[int64]struct{} // endpoints already tried in this request
	PrefixKey  []byte             // consistent-hashing key used by PrefixCacheFilter (complements SessionKey:
	//                              PrefixKey = stateless consistent hashing by content prefix; SessionKey = stateful
	//                              Redis affinity from an explicit client session id, combined with weighted+scoring)
}

// ErrorClass buckets upstream / network / protocol errors into a few coarse-grained classes.
//
// CooldownManager uses this to decide whether an endpoint should cool down and for how long.
type ErrorClass int

const (
	ClassUnknown   ErrorClass = iota // couldn't be classified
	ClassSuccess                     // 2xx
	ClassTransient                   // 5xx / network error / timeout / DNS
	ClassCapacity                    // upstream 429 / overloaded
	ClassPermanent                   // upstream 401 / 403 / config error
	ClassInvalid                     // client 4xx (other than 401/403/429); should not be retried
)

func (c ErrorClass) String() string {
	switch c {
	case ClassSuccess:
		return "success"
	case ClassTransient:
		return "transient"
	case ClassCapacity:
		return "capacity"
	case ClassPermanent:
		return "permanent"
	case ClassInvalid:
		return "invalid"
	default:
		return "unknown"
	}
}

// IsRetryable decides whether dispatch.RetryPolicy should keep Picking the next candidate.
//
//	Transient / Capacity / Permanent / Unknown → retry
//	Success / Invalid                          → stop
func (c ErrorClass) IsRetryable() bool {
	switch c {
	case ClassSuccess, ClassInvalid:
		return false
	default:
		return true
	}
}

// Result is the outcome of a single call, passed by the dispatcher (via SelectorAdapter) to Scheduler.Report.
type Result struct {
	Class    ErrorClass
	HTTPCode int           // upstream status; 0 = no response received (network error / timeout)
	Reason   string        // human-readable error description
	Latency  time.Duration // duration of this call (including upstream + streaming)

	// RetryAfter is the upstream's own recovery hint (parsed from Retry-After /
	// rate-limit reset headers on a failed response). When > 0, the cooldown
	// TTL uses this instead of the static per-class duration (clamped by the
	// CooldownManager implementation). 0 = no hint.
	RetryAfter time.Duration
}

// Scheduler is the entry point dispatch calls through SelectorAdapter. Stateless (docs/03 §4).
type Scheduler interface {
	// Pick takes the current model's candidates + exclude set, and outputs one endpoint.
	//
	// Returning nil means all candidates were filtered out (dispatch.FallbackPolicy.OnExhausted decides whether to abort with 503 or switch to the next model).
	Pick(ctx context.Context, req *Request) (*domain.Endpoint, error)

	// Report feeds this call's result back to cooldown / metric / stats store.
	// It does not decide subsequent control flow — dispatch.RetryPolicy.Decide looks at result.Class to decide whether to continue or stop.
	//
	// Report may be called more than once for a single Pick (e.g. a
	// supplementary StageStream verdict after a success), so it does **not**
	// touch the P2C pending-call counter — that is Release's job.
	Report(ctx context.Context, ep *domain.Endpoint, result Result)

	// Release marks the attempt for ep as finished, decrementing the P2C
	// pending-call counter exactly once. Pairs 1:1 with a Pick that returned a
	// non-nil ep. No-op when P2C tracking is not configured.
	Release(ctx context.Context, ep *domain.Endpoint)
}
