// Package metric 集中定义 Prometheus metric 命名常量与封装。
//
// 命名遵循 docs/08-observability.md §3：`llm_gateway_<subsystem>_<name>_<unit>`。
// 单位约定：seconds / total / ratio / bytes / count。
package metric

// === HTTP / Middleware 通用 metric（docs/08 §3）===
const (
	HTTPRequestsTotal          = "llm_gateway_http_requests_total"           // counter: method/route/status/error_class
	HTTPRequestDurationSeconds = "llm_gateway_http_request_duration_seconds" // histogram: method/route/status/model/routed_model
	MiddlewareDurationSeconds  = "llm_gateway_middleware_duration_seconds"
	MiddlewareErrorTotal       = "llm_gateway_middleware_error_total"
	ContextFieldMissTotal      = "llm_gateway_context_field_miss_total"
	PanicTotal                 = "llm_gateway_panic_total"
	RequestAbortedByShutdown   = "llm_gateway_request_aborted_by_shutdown_total"
)

// === Auth (M2) ===
const (
	AuthTotal = "llm_gateway_auth_total"
)

// === Budget (M4) ===
const (
	BudgetCheckTotal = "llm_gateway_budget_check_total"
)

// === RateLimit (M6) ===
const (
	RateLimitCheckTotal     = "llm_gateway_rate_limit_check_total"
	RateLimitConsumeTotal   = "llm_gateway_rate_limit_consume_total"
	RateLimitDecisionsTotal = "llm_gateway_ratelimit_decisions_total" // scope / dimension / result
	RateLimitChargeTotal    = "llm_gateway_ratelimit_charge_total"    // dimension / result
	RateLimitFailOpenTotal  = "llm_gateway_ratelimit_fail_open_total" // scope / dimension
	TPMOverflowTotal        = "llm_gateway_tpm_overflow_total"        // layer / dimension
	PolicyCacheTotal        = "llm_gateway_policy_cache_requests_total" // layer / result
	RateLimitOversellRatio  = "llm_gateway_rate_limit_oversell_ratio"
	RateLimitRejectionRate  = "llm_gateway_rate_limit_rejection_rate"
)

// === Schedule (M7 / docs/03) ===
const (
	SchedulerAttemptsTotal         = "llm_gateway_scheduler_attempts_total" // model / routed_model / vendor / endpoint_id / attempt_role / result / error_class
	SchedulerCandidates            = "llm_gateway_scheduler_candidates"     // histogram: model / stage
	SchedulingDurationSeconds      = "llm_gateway_scheduling_duration_seconds" // model / attempts
	EligibilityDurationSeconds     = "llm_gateway_eligibility_duration_seconds" // model
	SchedulerCooldownEnterTotal    = "llm_gateway_scheduler_cooldown_enter_total"
	ScheduleResultTotal            = "llm_gateway_schedule_result_total"
	SchedulerEndpointSelectedTotal = "llm_gateway_scheduler_endpoint_selected_total"
	SchedulerEndpointFilteredTotal = "llm_gateway_scheduler_endpoint_filtered_total"
	SchedulerEndpointCallTotal     = "llm_gateway_scheduler_endpoint_call_total"
)

// === Upstream (docs/03) ===
const (
	UpstreamRequestsTotal         = "llm_gateway_upstream_requests_total"          // vendor / endpoint_id / model / native_protocol / result / error_class
	UpstreamDurationSeconds       = "llm_gateway_upstream_duration_seconds"         // vendor / endpoint_id / model / result / error_class
	AdapterRequestTotal           = "llm_gateway_adapter_request_total"
	AdapterRequestDurationSeconds = "llm_gateway_adapter_request_duration_seconds"
	AdapterErrorTotal             = "llm_gateway_adapter_error_total"
	AdapterTranslateTotal         = "llm_gateway_adapter_translate_total"
)

// === Usage / Content Log (docs/05 + docs/08) ===
const (
	UsageTokensTotal              = "llm_gateway_usage_tokens_total"   // model / routed_model / vendor / direction
	UsagePublishTotal             = "llm_gateway_usage_publish_total"  // backend / result
	ContentLogPublishTotal        = "llm_gateway_content_log_publish_total" // backend / result / sampled
	OutboxBufferSize              = "llm_gateway_outbox_buffer_size"   // gauge: backend
	OutboxPublishDurationSeconds  = "llm_gateway_outbox_publish_duration_seconds" // driver / result
	OutboxDroppedTotal            = "llm_gateway_outbox_dropped_total" // driver / reason
	OutboxDLQTotal                = "llm_gateway_outbox_dlq_total"     // driver / result
	// dual-write 模式（driver=file_and_kafka）：file 是 source of truth，Kafka 是异步广播。
	// 两个 sink 各自的失败分开计数，便于告警：file_error 升 = 磁盘问题（严重）；
	// kafka_publish_error 升 = broker 问题（数据安全，由 replay 工具兜）。
	OutboxFileErrorTotal          = "llm_gateway_outbox_file_error_total" // dual-write: file sink 失败次数
	OutboxKafkaPublishErrorTotal  = "llm_gateway_outbox_kafka_publish_error_total" // dual-write: kafka sink 失败次数（file 已 commit）
	UsageExtractorSessionTotal    = "llm_gateway_usage_extractor_session_total"
)

// === Endpoint / Health (docs/08) ===
const (
	EndpointMisconfiguredTotal = "llm_gateway_endpoint_misconfigured_total" // vendor / reason
)
