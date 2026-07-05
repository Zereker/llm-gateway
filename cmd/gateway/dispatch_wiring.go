// dispatch_wiring.go: composition root that assembles dispatch.Dispatcher.
//
// **Zero business logic**—all dispatch port implementations live in
// pkg/dispatch/adapters (the primitive packages don't depend back on
// dispatch; composition happens in the adapters layer):
//
//	dispatch.CandidateSource ← cmd middleware_adapters.go adaptEndpoints (repo bridge)
//	dispatch.Selector        ← adapters.PickerAdapter (selector.Scheduler)
//	dispatch.InvokerFactory  ← adapters.InvokerFactoryAdapter (invoker.Sender)
//	dispatch.EndpointQuota   ← adapters.EndpointQuotaAdapter (ratelimit.Store + bucket helpers)
//	Policies                 ← built-in defaults from pkg/dispatch (HeaderAttemptCap / DefaultRetry / ModelChainFallback)
//
// cmd only "news up the types per the dependency graph and feeds them into dispatch.New".
package main

import (
	"github.com/zereker/llm-gateway/pkg/dispatch"
	"github.com/zereker/llm-gateway/pkg/dispatch/adapters"
	"github.com/zereker/llm-gateway/pkg/invoker"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
	"github.com/zereker/llm-gateway/pkg/selector"
	"github.com/zereker/llm-gateway/pkg/trace"
)

// buildDispatcher assembles dispatch.Dispatcher.
//
// Parameters:
//   - candidates  endpoint candidate source (bridged from repo via middleware_adapters.go adaptEndpoints)
//   - sched       selector.Scheduler implementation (filter chain + scorer + picker)
//   - sender      invoker.Sender (HTTP call + forward)
//   - rateStore   ratelimit.Store; nil = endpoint-quota noop
//   - maxAttempts AttemptCap default value
//   - tracer      trace.Tracer; nil = SlogTracer NoOp (dispatch's internal fallback)
func buildDispatcher(
	candidates dispatch.CandidateSource,
	sched selector.Scheduler,
	sender *invoker.Sender,
	rateStore ratelimit.Store,
	maxAttempts int,
	tracer trace.Tracer,
) *dispatch.Dispatcher {
	return dispatch.New(
		dispatch.WithCandidates(candidates),
		dispatch.WithSelector(adapters.NewSelector(sched)),
		dispatch.WithInvokerFactory(adapters.NewInvokerFactory(sender)),
		dispatch.WithQuota(adapters.NewEndpointQuota(rateStore)),
		dispatch.WithCap(dispatch.HeaderAttemptCap{Default: maxAttempts}),
		dispatch.WithRetry(dispatch.DefaultRetry{}),
		dispatch.WithFallback(dispatch.ModelChainFallback{}),
		dispatch.WithTracer(tracer),
	)
}
