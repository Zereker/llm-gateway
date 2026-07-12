package dispatch

import (
	"time"

	"github.com/zereker/llm-gateway/internal/failure"
)

// Verdict classifies the result of one Invoker.Invoke call.
//
// The fields follow selector.Result's semantics but belong to this
// package — shared vocabulary between Selector / Invoker / Policy, without
// depending on internal/schedule.
//
// **Stage field (added in v0.6)**: marks which stage of the dispatch
// pipeline this failure occurred in, letting Policy.Decide make more
// fine-grained decisions — e.g. a StagePrepare failure (pre-call protocol
// conversion failed) means the endpoint's endpoint.Protocol was picked
// wrong; retrying the same endpoint is pointless, so it can Switch directly
// to the next model or Abort.
type Verdict struct {
	Stage    Stage         // which stage this failure occurred in (added in v0.6); StageInvoke on success
	Class    Class         // coarse-grained classification (decides cooldown TTL + Decide → Action)
	HTTPCode int           // upstream status; 0 = no response obtained (network error / timeout)
	Reason   string        // human-readable error description
	Latency  time.Duration // this call's duration (including upstream + streaming)

	// RetryAfter is the upstream's own recovery hint (Retry-After / rate-limit
	// reset headers on the failed response); 0 = no hint. Selector.Report uses
	// it for reset-aware cooldown TTLs.
	RetryAfter time.Duration
}

// Stage marks the stage of the dispatcher pipeline — Policy uses this to
// tell "which step failed".
type Stage int

const (
	// StageInvoke is the upstream HTTP call stage (the default value —
	// success / network error / upstream 4xx/5xx all belong to this stage).
	StageInvoke Stage = iota
	// StageSelect is the endpoint-selection stage (selector dependency
	// failure / candidates exhausted).
	StageSelect
	// StagePrepare is the pre-call protocol-conversion stage (translator
	// failure / vendor HTTP construction failure). Note: DefaultRetry
	// currently decides on Class only and does not consume Stage — a
	// StagePrepare failure retries like any other Permanent (bounded by the
	// attempt cap). A Stage-aware policy could skip same-protocol endpoints
	// (they'd likely fail the same way), but that optimization is not
	// implemented; today Stage feeds spans / audit / stats attribution only.
	StagePrepare
	// StageReserve is the endpoint ratelimit pre-deduction stage (quota
	// exhausted).
	StageReserve
	// StageStream is the response-streaming stage (failure while forwarding
	// the body after an HTTP 200 — upstream RST / dropped mid-stream). The
	// HTTP status is already written, so it can't be rolled back and can't
	// be retried; this Stage exists purely for Selector.Report / stats: a
	// bad endpoint that "cuts off after 200" must be visible to cooldown /
	// scoring, otherwise it would show up as 100% success forever in the
	// stats.
	StageStream
)

func (s Stage) String() string {
	switch s {
	case StageSelect:
		return "select"
	case StagePrepare:
		return "prepare"
	case StageReserve:
		return "reserve"
	case StageInvoke:
		return "invoke"
	case StageStream:
		return "stream"
	default:
		return "unknown"
	}
}

// Class buckets upstream / network / protocol errors into 6 coarse-grained
// categories.
//
// **Semantics are unchanged** from selector.ErrorClass; the rename reflects
// renaming the "scheduling abstraction" to the "verdict abstraction" — Class
// is what the driver sees as "what kind of thing this call was", not "the
// scheduler's internal error type".
type Class = failure.Class

const (
	ClassUnknown   = failure.Unknown // couldn't be classified (IsRetryable = true, but Selector.Report doesn't write a cooldown)
	ClassSuccess   = failure.Success // 2xx + protocol-layer success
	ClassTransient = failure.Transient
	ClassCapacity  = failure.Capacity
	ClassPermanent = failure.Permanent
	ClassInvalid   = failure.Invalid
)

// IsRetryable reports whether this Class is worth retrying with a different
// endpoint.
//
//	Transient / Capacity / Permanent / Unknown → retry (a different endpoint
//	                                              might succeed)
//	Success / Invalid                          → no retry
//
// **Note**: Unknown is retryable, but doesn't write a cooldown (to avoid
// classification blind spots polluting cooldown state). That special
// handling happens inside Selector; Class.IsRetryable still follows the
// "should we switch endpoints" semantics.
