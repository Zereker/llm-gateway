// dispatch_wiring.go：composition root 装配 dispatch.Dispatcher。
//
// **零业务逻辑**——所有 dispatch port 实现都已归位到自家包：
//   - dispatch.Selector       ← pkg/selector.DispatchSelector
//   - dispatch.InvokerFactory ← pkg/invoker.DispatchInvokerFactory
//   - dispatch.EndpointQuota  ← pkg/ratelimit.EndpointQuota
//   - Policies                ← pkg/dispatch 内置默认（HeaderAttemptCap / DefaultRetry / ModelChainFallback）
//
// cmd 只做"按依赖图把 type new 出来塞进 dispatch.New"。
package main

import (
	"github.com/zereker/llm-gateway/pkg/dispatch"
	"github.com/zereker/llm-gateway/pkg/invoker"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
	"github.com/zereker/llm-gateway/pkg/selector"
)

// buildDispatcher 装配 dispatch.Dispatcher。
//
// 参数：
//   - endpoints   端点 SQL 读 port（middleware_adapters.go adaptEndpoints 桥接出来）
//   - sched       selector.Scheduler 实现（filter chain + scorer + picker）
//   - sender      invoker.Sender（HTTP 调用 + forward）
//   - rateStore   ratelimit.Store；nil = endpoint-quota noop
//   - maxAttempts AttemptCap default 值
func buildDispatcher(
	endpoints selector.EndpointReader,
	sched selector.Scheduler,
	sender *invoker.Sender,
	rateStore ratelimit.Store,
	maxAttempts int,
) *dispatch.Dispatcher {
	return dispatch.New(
		dispatch.WithSelector(selector.NewDispatchSelector(endpoints, sched)),
		dispatch.WithInvokerFactory(invoker.NewDispatchInvokerFactory(sender)),
		dispatch.WithQuota(ratelimit.NewEndpointQuota(rateStore)),
		dispatch.WithCap(dispatch.HeaderAttemptCap{Default: maxAttempts}),
		dispatch.WithRetry(dispatch.DefaultRetry{}),
		dispatch.WithFallback(dispatch.ModelChainFallback{}),
	)
}
