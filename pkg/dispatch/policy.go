package dispatch

import "github.com/zereker/llm-gateway/pkg/domain"

// AttemptCap 决定本请求的最大 attempt 数。
//
// 默认实现 HeaderAttemptCap：cfg 默认值 + X-Gateway-Max-Attempts header
// 只允许往更紧（更小）的方向覆盖。
type AttemptCap interface {
	Resolve(rc *domain.RequestContext) int
}

// RetryPolicy 一次 Invoker.Invoke 完成后，决定 driver loop 的下一步。
//
// 输入：state（read-only 投影）+ verdict（本次调用结果）
// 输出：Action（Continue / Stream / Abort，不返 Switch——切 model 由 FallbackPolicy 管）
//
// **默认实现** DefaultRetry：按 Class.IsRetryable 决定。
// **扩展空间**：cost-aware retry / circuit breaker / exponential backoff
// 都是实现新的 RetryPolicy，不动 Dispatcher。
type RetryPolicy interface {
	Decide(s State, v Verdict) Action
}

// FallbackPolicy 当前 model 候选耗尽（Selector.Select 返 nil）时，决定下一步。
//
// 输入：state（含 RemainingModels）
// 输出：Action（Switch / Abort，不返 Continue / Stream）
//
// **默认实现** ModelChainFallback：按 rc.ModelChain 顺序切。
// **扩展空间**：race fallback（并发试多个 model）/ weighted fallback 等。
type FallbackPolicy interface {
	OnExhausted(s State) Action
}
