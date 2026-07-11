package dispatch

// ModelChainFallback switches to the next fallback model in rc.ModelChain
// order.
//
// Behavior:
//
//	state.NextFallback() found → Switch{Next: next}
//	state.NextFallback() empty → Abort{NoEndpoint, 503}
//
// **Replaceable**: implement the FallbackPolicy interface to write a new
// strategy — race fallback (try multiple models concurrently) / weighted
// fallback / cost-aware fallback, etc.
type ModelChainFallback struct{}

// OnExhausted decides what to do when the current model's candidates are
// exhausted.
func (ModelChainFallback) OnExhausted(s State) Action {
	next, ok := s.NextFallback()
	if !ok {
		return Abort{
			Result:   OutcomeNoEndpoint,
			Class:    ClassUnknown,
			HTTPCode: 503,
			Reason:   "no endpoint available across all models",
		}
	}
	return Switch{Next: next}
}
