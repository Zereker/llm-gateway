package dispatch

import "github.com/zereker/llm-gateway/internal/trace"

// Option configures optional settings on a Dispatcher.
type Option func(*Dispatcher)

// WithCandidates injects a CandidateSource implementation. Required.
//
// Typical implementation: internal/app/gateway/adapters.go's
// adaptEndpoints bridges repo.EndpointReader into dispatch.CandidateSource.
func WithCandidates(c CandidateSource) Option {
	return func(d *Dispatcher) { d.candidates = c }
}

// WithSelector injects a Selector implementation. Required.
//
// See internal/dispatch/adapters.PickerAdapter for the default implementation
// (wraps selector.Scheduler.Pick + Report).
func WithSelector(s Selector) Option {
	return func(d *Dispatcher) { d.selector = s }
}

// WithInvokerFactory injects an InvokerFactory implementation. Required.
func WithInvokerFactory(f InvokerFactory) Option {
	return func(d *Dispatcher) { d.invokerFactory = f }
}

// WithCap injects an AttemptCap policy. Required.
//
// See HeaderAttemptCap (cap_header.go) for the default implementation.
func WithCap(c AttemptCap) Option {
	return func(d *Dispatcher) { d.cap = c }
}

// WithRetry injects a RetryPolicy. Required.
//
// See DefaultRetry (retry_default.go) for the default implementation.
func WithRetry(r RetryPolicy) Option {
	return func(d *Dispatcher) { d.retry = r }
}

// WithFallback injects a FallbackPolicy. Required.
//
// See ModelChainFallback (fallback_chain.go) for the default implementation.
func WithFallback(f FallbackPolicy) Option {
	return func(d *Dispatcher) { d.fallback = f }
}

// WithQuota injects an EndpointQuota implementation. **Optional** — not
// calling this leaves NoopQuota, which never rejects.
//
// See internal/dispatch/adapters.EndpointQuotaAdapter for a typical implementation
// (wraps ratelimit.Store plus ratelimit's own endpoint bucket-key derivation
// helper, internal/ratelimit/endpoint_buckets.go).
func WithQuota(q EndpointQuota) Option {
	return func(d *Dispatcher) { d.quota = q }
}

// WithTracer injects a trace.Tracer. Optional — not calling this leaves
// NewSlogTracer(nil), a NoOp span.
//
// To wire up OTel: cmd/gateway feeds in trace.NewOtelTracer, and the
// dispatcher opens a span on every Dispatch / attempt (dispatch.request →
// dispatch.attempt child span), with span attributes covering
// model / endpoint / verdict / outcome and events covering fallback
// switches.
func WithTracer(t trace.Tracer) Option {
	return func(d *Dispatcher) { d.tracer = t }
}
