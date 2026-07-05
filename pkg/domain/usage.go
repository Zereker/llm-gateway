package domain

import (
	"encoding/json"
	"time"
)

// Usage is the resource-consumption event for a single request.
//
// Defined per docs/architecture/05-metering-billing.md §3; the downstream
// billing platform decides its own pricing rules from `Raw` + (vendor, model,
// protocol, request time) — the gateway does not maintain an enumeration of
// vendor-specific fields.
//
// **Source / Estimator / Confidence**: identify the origin and trustworthiness
// of the usage — an estimated value is never disguised as an authoritative
// upstream value.
//
//	upstream  / exact       : upstream returned native usage
//	extracted / derived     : translator pulled it out of the response
//	estimated / approximate : tokenizer or char-count fallback
//
// **Raw**: the raw upstream usage JSON, forwarded as-is to the billing
// platform. Raw is kept even when translator fails to extract the common
// fields, so downstream can still parse it by its own rules.
//
// **Truncated**: true when a streaming response was cut off mid-way / the
// client closed the connection; downstream can decide whether to trust this
// usage event based on Confidence.
type Usage struct {
	Input     int64 `json:"input"`               // generic input token count; usually prompt + system message (including cached portion)
	Output    int64 `json:"output"`              // generic output token count
	Total     int64 `json:"total"`               // total; authoritative when present, otherwise = Input + Output
	Truncated bool  `json:"truncated,omitempty"` // whether the response did not complete fully

	Raw json.RawMessage `json:"raw,omitempty"` // raw upstream usage object (passed through to downstream billing)

	// Source / Estimator / Confidence — identify the origin and trustworthiness of usage
	Source     UsageSource     `json:"source,omitempty"`     // upstream | extracted | estimated
	Estimator  UsageEstimator  `json:"estimator,omitempty"`  // tiktoken | naive_chars | vendor_default | ""
	Confidence UsageConfidence `json:"confidence,omitempty"` // exact | derived | approximate

	Meta UsageMeta `json:"meta"`
}

// UsageSource identifies how the usage field was obtained.
type UsageSource string

const (
	UsageSourceUpstream  UsageSource = "upstream"  // upstream returned native usage
	UsageSourceExtracted UsageSource = "extracted" // translator parsed it from response fields
	UsageSourceEstimated UsageSource = "estimated" // tokenizer / char-count estimate
	UsageSourceCache     UsageSource = "cache"     // response cache hit; original usage passed through (downstream can bill it at zero cost)
)

// UsageEstimator is the algorithm used for estimation (set when Source=estimated).
type UsageEstimator string

const (
	UsageEstimatorNone          UsageEstimator = ""               // not an estimation path
	UsageEstimatorTiktoken      UsageEstimator = "tiktoken"       // OpenAI tiktoken
	UsageEstimatorNaiveChars    UsageEstimator = "naive_chars"    // rough estimate by character count
	UsageEstimatorVendorDefault UsageEstimator = "vendor_default" // vendor-provided tokenizer
)

// UsageConfidence is the trustworthiness of the field.
type UsageConfidence string

const (
	UsageConfidenceExact       UsageConfidence = "exact"       // exact figure from upstream
	UsageConfidenceDerived     UsageConfidence = "derived"     // parsed by translator
	UsageConfidenceApproximate UsageConfidence = "approximate" // estimated
)

// UsageMeta is metering-event metadata used by the billing platform to
// correlate identity / model / routing / request time.
//
// See docs/architecture/05-metering-billing.md §4 for field provenance.
type UsageMeta struct {
	AccountID    string `json:"account_id"` // parent account pin / billing subject (written by M2)
	Model        string `json:"model"`      // actually-routed model; takes RoutedModelService.Model on cross-model fallback
	Vendor       string `json:"vendor"`     // endpoint vendor
	EndpointID   string `json:"endpoint_id"`
	SubAccountID string `json:"sub_account_id"` // sub-account / operator
	APIKeyID     string `json:"api_key_id"`
	ServiceID    string `json:"service_id,omitempty"` // model_services.service_id (a renamable string)

	// ModelServiceID — downstream billing uses (account_id, model_service_id,
	// rule_class, StartTime) to hit the idx_active_lookup index on
	// pricing_versions and select the effective price row
	// (effective_from <= StartTime < effective_to). Taken from
	// RoutedModelService (the model actually billed after fallback), same as
	// Model / ServiceID.
	ModelServiceID int64 `json:"model_service_id,omitempty"`

	// ServiceUpdateTime — a snapshot of model_services.updated_at, for
	// **diagnostic reference only** (the catalog version the gateway saw
	// when the event was generated; may lag by ≤30s under the repo cache).
	//
	// **Not a pricing lookup key**: pricing_versions has no such column, and
	// a price change (append-only INSERT) does not touch
	// model_services.updated_at — price matching always goes through the
	// effective_from/to range keyed on StartTime (docs/05 §6: the gateway
	// does not do price resolution; time semantics belong to downstream).
	ServiceUpdateTime time.Time `json:"service_update_time,omitempty"`

	RequestID    string    `json:"request_id"`
	TraceID      string    `json:"trace_id,omitempty"`
	StartTime    time.Time `json:"start_time"`
	EndTime      time.Time `json:"end_time"`
	TTFTMs       int64     `json:"ttft_ms,omitempty"`       // time to first byte for streaming responses; 0 for non-streaming
	TotalLatency int64     `json:"total_latency,omitempty"` // gateway end-to-end latency, ms
}
