// Package metric centralizes the Prometheus metric naming constants and wrapper.
//
// Naming follows docs/08-observability.md §3: `llm_gateway_<subsystem>_<name>_<unit>`.
// Unit convention: seconds / total / ratio / bytes / count.
package metric

// === HTTP / Middleware common metrics (docs/08 §3) ===
const (
	HTTPRequestsTotal          = "llm_gateway_http_requests_total"           // counter: method/route/status/error_class
	HTTPRequestDurationSeconds = "llm_gateway_http_request_duration_seconds" // histogram: method/route/status/model/routed_model
	ResponseTTFTSeconds        = "llm_gateway_response_ttft_seconds"         // histogram: model/routed_model/vendor
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

// === Policy enforcement (M8) ===
const (
	PolicyDecisionsTotal   = "llm_gateway_policy_decisions_total"   // stage / action
	PolicyEnforcementTotal = "llm_gateway_policy_enforcement_total" // stage / action / result
)

// === RateLimit (M6) ===
const (
	RateLimitCheckTotal     = "llm_gateway_rate_limit_check_total"
	RateLimitConsumeTotal   = "llm_gateway_rate_limit_consume_total"
	RateLimitDecisionsTotal = "llm_gateway_ratelimit_decisions_total"   // scope / dimension / result
	RateLimitChargeTotal    = "llm_gateway_ratelimit_charge_total"      // dimension / result
	RateLimitFailOpenTotal  = "llm_gateway_ratelimit_fail_open_total"   // scope / dimension
	TPMOverflowTotal        = "llm_gateway_tpm_overflow_total"          // layer / dimension
	PolicyCacheTotal        = "llm_gateway_policy_cache_requests_total" // layer / result
	RateLimitOversellRatio  = "llm_gateway_rate_limit_oversell_ratio"
	RateLimitRejectionRate  = "llm_gateway_rate_limit_rejection_rate"
)

// === Schedule (M7 / docs/03) ===
const (
	InvokerAttemptsTotal          = "llm_gateway_invoker_attempts_total"        // model / routed_model / vendor / endpoint_id / attempt_role / result / error_class
	SelectorCandidates            = "llm_gateway_selector_candidates"           // histogram: model / stage
	SchedulingDurationSeconds     = "llm_gateway_scheduling_duration_seconds"   // model / attempts
	EligibilityDurationSeconds    = "llm_gateway_eligibility_duration_seconds"  // model
	SelectorCooldownEnterTotal    = "llm_gateway_selector_cooldown_enter_total" // endpoint_id / vendor / class
	ScheduleResultTotal           = "llm_gateway_schedule_result_total"
	SelectorEndpointSelectedTotal = "llm_gateway_selector_endpoint_selected_total" // endpoint_id / vendor / model
	SelectorEndpointFilteredTotal = "llm_gateway_selector_endpoint_filtered_total"
	SelectorEndpointCallTotal     = "llm_gateway_selector_endpoint_call_total" // endpoint_id / vendor / model / outcome / class
	RoutingDecisionsTotal         = "llm_gateway_routing_decisions_total"      // outcome / reason / scope_kind
)

// === Upstream (docs/03) ===
const (
	InvokerRequestsTotal          = "llm_gateway_invoker_requests_total"   // vendor / endpoint_id / model / protocol / result / error_class
	InvokerDurationSeconds        = "llm_gateway_invoker_duration_seconds" // vendor / endpoint_id / model / result / error_class
	AdapterRequestTotal           = "llm_gateway_adapter_request_total"
	AdapterRequestDurationSeconds = "llm_gateway_adapter_request_duration_seconds"
	AdapterErrorTotal             = "llm_gateway_adapter_error_total"
	AdapterTranslateTotal         = "llm_gateway_adapter_translate_total"
)

// === Usage / Content Log (docs/05 + docs/08) ===
const (
	UsageTokensTotal             = "llm_gateway_usage_tokens_total"              // model / routed_model / vendor / direction
	UsagePublishTotal            = "llm_gateway_usage_publish_total"             // backend / result
	ContentLogPublishTotal       = "llm_gateway_content_log_publish_total"       // backend / result / sampled
	OutboxBufferSize             = "llm_gateway_outbox_buffer_size"              // gauge: backend
	OutboxPublishDurationSeconds = "llm_gateway_outbox_publish_duration_seconds" // driver / result
	OutboxDroppedTotal           = "llm_gateway_outbox_dropped_total"            // driver / reason
	OutboxDLQTotal               = "llm_gateway_outbox_dlq_total"                // driver / result
	// dual-write mode (driver=file_and_kafka): file is the source of truth, Kafka is the async broadcast.
	// The two sinks' failures are counted separately to make alerting clearer: a rise in file_error means
	// a disk problem (severe); a rise in kafka_publish_error means a broker problem (data is still safe,
	// covered by the replay tool).
	OutboxFileErrorTotal         = "llm_gateway_outbox_file_error_total"          // dual-write: file sink failure count
	OutboxKafkaPublishErrorTotal = "llm_gateway_outbox_kafka_publish_error_total" // dual-write: kafka sink failure count (file already committed)
	UsageExtractorSessionTotal   = "llm_gateway_usage_extractor_session_total"
)

// === Endpoint / Health (docs/08) ===
const (
	EndpointMisconfiguredTotal = "llm_gateway_endpoint_misconfigured_total" // vendor / reason
)

// === Protocol translation (docs/02) ===
const (
	// TranslatorFeatureDroppedTotal counts request features a text-only
	// cross-protocol translator drops (labels: src / tgt / feature, where
	// feature is tools | tool_calls | multimodal).
	TranslatorFeatureDroppedTotal = "llm_gateway_translator_feature_dropped_total"
)

// === Response Cache (docs/08) ===
const (
	ResponseCacheTotal = "llm_gateway_response_cache_total" // result = hit | miss | store | bypass
)

// === Repo Cache (docs/08) ===
const (
	RepoCacheTotal = "llm_gateway_repo_cache_total" // counter: table / result (hit / miss / error)
)
