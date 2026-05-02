// Package metric 集中定义 Prometheus metric 命名常量与封装。
//
// 命名规约：ai_gateway.<component>.<name>{labels}
// 各业务包通过 metric.Inc / metric.Observe 等 API 走本包。
package metric

// === Middleware 通用 metric ===
const (
	HTTPRequestDurationMs = "ai_gateway.http.request_duration_ms"
	MiddlewareDurationMs  = "ai_gateway.middleware.duration_ms"
	MiddlewareErrorTotal  = "ai_gateway.middleware.error_total"
	ContextFieldMissTotal = "ai_gateway.context.field_miss_total"
	PanicTotal            = "ai_gateway.panic_total"
)

// === Auth (M2) ===
const (
	AuthTotal = "ai_gateway.auth.total"
)

// === Budget (M4) ===
const (
	BudgetCheckTotal = "ai_gateway.budget.check_total"
)

// === RateLimit (M6 / docs/architecture/04) ===
const (
	RateLimitCheckTotal    = "ai_gateway.rate_limit.check_total"
	RateLimitConsumeTotal  = "ai_gateway.rate_limit.consume_total"
	RateLimitOversellRatio = "ai_gateway.rate_limit.oversell_ratio"
	RateLimitRejectionRate = "ai_gateway.rate_limit.rejection_rate"
)

// === Schedule (M7 / docs/architecture/03) ===
const (
	ScheduleResultTotal            = "ai_gateway.schedule.result_total"
	SchedulerEndpointSelectedTotal = "ai_gateway.scheduler.endpoint.selected_total"
	SchedulerEndpointFilteredTotal = "ai_gateway.scheduler.endpoint.filtered_total"
	SchedulerEndpointCallTotal     = "ai_gateway.scheduler.endpoint.call_total"
	SchedulerCooldownEnterTotal    = "ai_gateway.scheduler.cooldown.enter_total"
)

// === Adapter (docs/architecture/02) ===
const (
	AdapterRequestTotal      = "ai_gateway.adapter.request_total"
	AdapterRequestDurationMs = "ai_gateway.adapter.request_duration_ms"
	AdapterErrorTotal        = "ai_gateway.adapter.error_total"
	AdapterTranslateTotal    = "ai_gateway.adapter.translate_total"
)

// === Usage / Pricing (docs/architecture/05) ===
const (
	UsageExtractorSessionTotal = "ai_gateway.usage.extractor.session_total"
	UsageBusPublishTotal       = "ai_gateway.usage.bus.publish_total"
	UsageLocalLogWriteTotal    = "ai_gateway.usage.locallog.write_total"
	PricingLookupTotal         = "ai_gateway.pricing.lookup_total"
)
