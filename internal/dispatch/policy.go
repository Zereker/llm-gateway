package dispatch

// AttemptCap decides the maximum number of attempts for this request.
//
// Default implementation HeaderAttemptCap: cfg default + the
// X-Gateway-Max-Attempts header, which is only allowed to override in the
// tighter (smaller) direction.
//
// **The input is Input, not RC**: dispatch never touches RequestContext;
// client-header-style overrides are parsed by middleware and passed through
// via Input.AttemptCapOverride.
type AttemptCap interface {
	Resolve(in Input) int
}

// RetryPolicy decides the driver loop's next step after one
// Invoker.Invoke completes.
//
// Input: state (a read-only projection) + verdict (this call's result)
// Output: Action (Continue / Stream / Abort — never Switch, model switching
// is FallbackPolicy's job)
//
// **Default implementation** DefaultRetry: decides based on
// Class.IsRetryable.
// **Extension point**: cost-aware retry / circuit breaker / exponential
// backoff are all just new RetryPolicy implementations, without touching
// Dispatcher.
type RetryPolicy interface {
	Decide(s State, v Verdict) Action
}

// FallbackPolicy decides the next step when the current model's candidates
// are exhausted (Selector.Select returned nil).
//
// Input: state (including the next fallback model, when present)
// Output: Action (Switch / Abort — never Continue / Stream)
//
// **Default implementation** ModelChainFallback: switches in rc.ModelChain
// order.
// **Extension point**: race fallback (try multiple models concurrently) /
// weighted fallback, etc.
type FallbackPolicy interface {
	OnExhausted(s State) Action
}
