// dispatch_wiring.go：composition root 装配 dispatch.Dispatcher。
//
// **零业务逻辑**——所有 dispatch port 实现都集中在 pkg/dispatch/adapters
// （primitive 包不反向依赖 dispatch，由 adapters 层做组合）：
//
//	dispatch.Selector       ← adapters.SelectorAdapter (CandidateSource + Scheduler + eligibility)
//	dispatch.InvokerFactory ← adapters.InvokerFactoryAdapter (invoker.Sender)
//	dispatch.EndpointQuota  ← adapters.EndpointQuotaAdapter (ratelimit.Store + bucket helpers)
//	Policies                ← pkg/dispatch 内置默认（HeaderAttemptCap / DefaultRetry / ModelChainFallback）
//
// cmd 只做"按依赖图把 type new 出来塞进 dispatch.New"。
package main

import (
	"github.com/zereker/llm-gateway/pkg/dispatch"
	"github.com/zereker/llm-gateway/pkg/dispatch/adapters"
	"github.com/zereker/llm-gateway/pkg/invoker"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
	"github.com/zereker/llm-gateway/pkg/selector"
)

// buildDispatcher 装配 dispatch.Dispatcher。
//
// 参数：
//   - candidates  端点候选源（middleware_adapters.go adaptEndpoints 桥接 repo 出来）
//   - sched       selector.Scheduler 实现（filter chain + scorer + picker）
//   - sender      invoker.Sender（HTTP 调用 + forward）
//   - rateStore   ratelimit.Store；nil = endpoint-quota noop
//   - maxAttempts AttemptCap default 值
func buildDispatcher(
	candidates dispatch.CandidateSource,
	sched selector.Scheduler,
	sender *invoker.Sender,
	rateStore ratelimit.Store,
	maxAttempts int,
) *dispatch.Dispatcher {
	return dispatch.New(
		dispatch.WithSelector(adapters.NewSelector(candidates, sched)),
		dispatch.WithInvokerFactory(adapters.NewInvokerFactory(sender)),
		dispatch.WithQuota(adapters.NewEndpointQuota(rateStore)),
		dispatch.WithCap(dispatch.HeaderAttemptCap{Default: maxAttempts}),
		dispatch.WithRetry(dispatch.DefaultRetry{}),
		dispatch.WithFallback(dispatch.ModelChainFallback{}),
	)
}
