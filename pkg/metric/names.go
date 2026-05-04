// Package metric 集中定义 Prometheus metric 命名常量与封装。
//
// **命名约定**：直接 Prometheus-native 下划线格式 `<namespace>_<subsystem>_<name>_<unit>`，
// 跟 https://prometheus.io/docs/practices/naming/ 完全一致。本包**不**做任何 string
// rewrite，const 值字面就是 Prometheus 端展示的 name。
//
// **单位约定**：
//   - 时间：`_seconds`（base SI unit；Prometheus 官方推荐，比 _ms 更通用）
//   - 计数：`_total`（counter）
//   - 比率：`_ratio`（0-1 区间）
//   - 字节：`_bytes`
//
// **强制使用 const**：业务代码 `metric.Inc(metric.AuthTotal, ...)`，**不要**写
// `metric.Inc("ai_gateway_auth_total", ...)`——字面量分散后改名找不到 / typo 难发现。
//
// 加新 metric：在本文件加 const → 业务代码引用。如果暂时不想加 const 就别加 metric，
// 反之命名会回到散落字面量的混乱状态。
package metric

// === Middleware 通用 metric ===
const (
	HTTPRequestDurationSeconds = "ai_gateway_http_request_duration_seconds"
	MiddlewareDurationSeconds  = "ai_gateway_middleware_duration_seconds"
	MiddlewareErrorTotal       = "ai_gateway_middleware_error_total"
	ContextFieldMissTotal      = "ai_gateway_context_field_miss_total"
	PanicTotal                 = "ai_gateway_panic_total"
)

// === Auth (M2) ===
const (
	AuthTotal = "ai_gateway_auth_total"
)

// === Budget (M4) ===
const (
	BudgetCheckTotal = "ai_gateway_budget_check_total"
)

// === RateLimit (M6 / docs/architecture/04) ===
const (
	RateLimitCheckTotal    = "ai_gateway_rate_limit_check_total"
	RateLimitConsumeTotal  = "ai_gateway_rate_limit_consume_total"
	RateLimitOversellRatio = "ai_gateway_rate_limit_oversell_ratio"
	RateLimitRejectionRate = "ai_gateway_rate_limit_rejection_rate"
)

// === Schedule (M7 / docs/architecture/03) ===
const (
	ScheduleResultTotal            = "ai_gateway_schedule_result_total"
	SchedulerEndpointSelectedTotal = "ai_gateway_scheduler_endpoint_selected_total"
	SchedulerEndpointFilteredTotal = "ai_gateway_scheduler_endpoint_filtered_total"
	SchedulerEndpointCallTotal     = "ai_gateway_scheduler_endpoint_call_total"
	SchedulerCooldownEnterTotal    = "ai_gateway_scheduler_cooldown_enter_total"
)

// === Adapter (docs/architecture/02) ===
const (
	AdapterRequestTotal           = "ai_gateway_adapter_request_total"
	AdapterRequestDurationSeconds = "ai_gateway_adapter_request_duration_seconds"
	AdapterErrorTotal             = "ai_gateway_adapter_error_total"
	AdapterTranslateTotal         = "ai_gateway_adapter_translate_total"
)

// === Usage / Pricing (docs/architecture/05) ===
const (
	UsageExtractorSessionTotal = "ai_gateway_usage_extractor_session_total"
	UsageBusPublishTotal       = "ai_gateway_usage_bus_publish_total"
	UsageBusQueueDepth         = "ai_gateway_usage_bus_queue_depth"   // gauge
	UsageBusDroppedTotal       = "ai_gateway_usage_bus_dropped_total" // counter
	UsageLocalLogWriteTotal    = "ai_gateway_usage_locallog_write_total"
	PricingLookupTotal         = "ai_gateway_pricing_lookup_total"
)
