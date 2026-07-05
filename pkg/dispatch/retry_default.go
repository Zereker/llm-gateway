package dispatch

// DefaultRetry is the current default retry policy: it translates one
// attempt's Verdict into an Action the Dispatcher's outer reducer can
// consume.
//
// Mapping rules:
//
//	Success     → Stream{}            (lets Dispatcher call res.StreamTo)
//	Invalid     → Abort{400}          (translator failure / client error; no retry)
//	Retryable   → Continue{}          (Transient / Capacity / Permanent / Unknown)
//	Non-retry   → Abort{502}          (fallback; in theory already covered by IsRetryable)
//
// **Replaceable**: implement the RetryPolicy interface to write a new
// policy — cost-aware retry / circuit breaker / exponential backoff /
// time-window budget, etc.
type DefaultRetry struct{}

// Decide translates a Verdict into an Action.
func (DefaultRetry) Decide(_ State, v Verdict) Action {
	switch v.Class {
	case ClassSuccess:
		return Stream{}
	case ClassInvalid:
		return Abort{
			Result:   OutcomeInvalid,
			Class:    v.Class,
			HTTPCode: 400,
			Reason:   v.Reason,
		}
	}
	if v.Class.IsRetryable() {
		return Continue{}
	}
	return Abort{
		Result:   OutcomeTerminal,
		Class:    v.Class,
		HTTPCode: 502,
		Reason:   v.Reason,
	}
}
