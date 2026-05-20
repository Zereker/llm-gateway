package dispatch

// ModelChainFallback 按 rc.ModelChain 顺序切下一个 fallback model。
//
// 行为：
//
//	state.RemainingModels() 非空 → Switch{Next: rem[0]}
//	state.RemainingModels() 空 → Abort{NoEndpoint, 503}
//
// **可替换**：实现 FallbackPolicy 接口写新策略——race fallback（并发试多个 model）/
// weighted fallback / cost-aware fallback 等。
type ModelChainFallback struct{}

// OnExhausted 当前 model 候选耗尽时的决定。
func (ModelChainFallback) OnExhausted(s State) Action {
	rem := s.RemainingModels()
	if len(rem) == 0 {
		return Abort{
			Result:   OutcomeNoEndpoint,
			Class:    ClassUnknown,
			HTTPCode: 503,
			Reason:   "no endpoint available across all models",
		}
	}
	return Switch{Next: rem[0]}
}
