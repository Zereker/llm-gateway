package dispatch

import "github.com/zereker/llm-gateway/pkg/trace"

// Option 装配 Dispatcher 的可选项。
type Option func(*Dispatcher)

// WithCandidates 注入 CandidateSource 实现。必填。
//
// 典型实现：cmd/gateway/middleware_adapters.go adaptEndpoints
// 把 repo.EndpointReader 桥接成 dispatch.CandidateSource。
func WithCandidates(c CandidateSource) Option {
	return func(d *Dispatcher) { d.candidates = c }
}

// WithSelector 注入 Selector 实现。必填。
//
// 默认实现见 pkg/dispatch/adapters.PickerAdapter（wrap selector.Scheduler.Pick + Report）。
func WithSelector(s Selector) Option {
	return func(d *Dispatcher) { d.selector = s }
}

// WithInvokerFactory 注入 InvokerFactory 实现。必填。
func WithInvokerFactory(f InvokerFactory) Option {
	return func(d *Dispatcher) { d.invokerFactory = f }
}

// WithCap 注入 AttemptCap 策略。必填。
//
// 默认实现见 HeaderAttemptCap（cap_header.go）。
func WithCap(c AttemptCap) Option {
	return func(d *Dispatcher) { d.cap = c }
}

// WithRetry 注入 RetryPolicy。必填。
//
// 默认实现见 DefaultRetry（retry_default.go）。
func WithRetry(r RetryPolicy) Option {
	return func(d *Dispatcher) { d.retry = r }
}

// WithFallback 注入 FallbackPolicy。必填。
//
// 默认实现见 ModelChainFallback（fallback_chain.go）。
func WithFallback(f FallbackPolicy) Option {
	return func(d *Dispatcher) { d.fallback = f }
}

// WithQuota 注入 EndpointQuota 实现。**可选**——不调 = NoopQuota 永不拒绝。
//
// 典型实现见 pkg/dispatch/adapters.EndpointQuotaAdapter（包装 ratelimit.Store +
// ratelimit 自带的 endpoint bucket key 派生 helper，pkg/ratelimit/endpoint_buckets.go）。
func WithQuota(q EndpointQuota) Option {
	return func(d *Dispatcher) { d.quota = q }
}

// WithTracer 注入 trace.Tracer。可选——不调 = NewSlogTracer(nil) NoOp span。
//
// 接入 OTel：cmd/gateway 把 trace.NewOtelTracer 喂进来，dispatcher 会在每次
// Dispatch / attempt 上开 span（dispatch.request → dispatch.attempt 子 span），
// span attribute 含 model / endpoint / verdict / outcome，事件含 fallback 切换。
func WithTracer(t trace.Tracer) Option {
	return func(d *Dispatcher) { d.tracer = t }
}
