// Package dispatch 协调 Selector + Invoker，把一次请求路由到合适的 endpoint
// 并执行（含 model fallback 链 + attempt cap + retry 策略）。
//
// **设计精神**：M7 middleware 是 framework-thin adapter；业务编排（retry /
// fallback / verdict 决策）全部在本包内，与 gin/echo/chi 无关。
//
// **四个角色**：
//
//	Dispatcher  ── 业务编排 + Action 消费循环（本包）
//	Selector    ── 选 endpoint 抽象（pkg/selector，目前 = pkg/schedule）
//	Invoker     ── 调下游抽象（pkg/invoker，目前 = pkg/upstream）
//	Policy×3    ── 决策点策略：AttemptCap / RetryPolicy / FallbackPolicy（本包）
//
// **driver loop 形态**：
//
//	for {
//	    switch a := dispatcher.step(...).(type) {
//	    case Continue: ...
//	    case Switch:   ...
//	    case Stream:   ...
//	    case Abort:    ...
//	    }
//	}
//
// 业务真相分布在 Policy 实现里。Dispatcher 只是 Action 的 reducer。
//
// 详见 docs/architecture/03a-schedule-overview.md（调度 + dispatch 编排总览）
// 和 docs/architecture/03-endpoint-scheduling.md（endpoint 选择 / cooldown / retry）。
package dispatch
