package dispatch

// Option 装配 Dispatcher 的可选项。
type Option func(*Dispatcher)

// WithSelector 注入 Selector 实现。必填。
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
// 典型实现见 pkg/ratelimit.EndpointQuota（包装 ratelimit.Store + selector 提供的
// bucket key 派生 helper）。
func WithQuota(q EndpointQuota) Option {
	return func(d *Dispatcher) { d.quota = q }
}
