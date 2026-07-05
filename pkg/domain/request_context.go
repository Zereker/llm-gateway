package domain

import (
	"time"
)

// RequestContext is the full-pipeline mutable state for a single HTTP request.
//
// See docs/architecture/01-request-pipeline.md §3 for field definitions.
//
// Write rules:
//   - Each field is annotated with the middleware (M1-M10) that writes it
//   - A later-registered middleware should not overwrite a field already written by an earlier one (unless a comment explicitly allows it)
//   - Handler / Adapter are treated as read-only consumers; Usage / Error / SchedulingDecision are response-phase products
//
// Read rules:
//   - Obtained via pkg/middleware.GetRequestContext(c); bare c.MustGet/c.Get calls are forbidden
type RequestContext struct {
	// === Written by M1 TraceContext ===
	//
	// trace_id / span_id are **not** RC fields — they belong to the OTel
	// SpanContext; use middleware.TraceIDFromCtx to extract the string form.
	// This avoids a dual-source drift.
	RequestID string // shaped like req_<12hex>; unique per request; used to locate client-reported issues
	StartTime time.Time

	// === Written by M2 Auth ===
	Identity UserIdentity

	// === Written by M3 Envelope ===
	Envelope *RequestEnvelope

	// === Default written by M3 Envelope; later middleware may override it (multi-tenant / canary scenarios) ===
	//
	// Type = protocol.Lookup (using any to avoid a pkg/domain → pkg/dispatch →
	// pkg/protocol → pkg/protocol → pkg/domain import cycle). Access via the
	// type-safe helper middleware.HandlersFrom(rc); don't type-assert directly.
	//
	// **v0.6 merge**: v0.5 originally hung two independent lookups, adapter
	// and translator, on the RC (rc.Adapters + rc.Translators), requiring
	// two lookups on the consumer side; v0.6 merged them into a single
	// protocol.Lookup, so a consumer only needs one (endpoint, srcProto) → Handler lookup.
	//
	// The default value protocol.DefaultLookup wraps the global adapter + translator registry.
	Handlers any

	// === Written by M5 ModelService (the originally requested model) ===
	//
	// **Important**: M5 does not query active pricing (docs/01 §7, docs/05
	// §6). Pricing matching is done by the downstream billing platform based
	// on the Usage Event's RequestTime; the gateway does not maintain a
	// PricingSnapshot.
	ModelService *ModelService

	// === Written by M5 ModelService (the pre-resolved attempt sequence) ===
	//
	// ModelChain[0] = primary (== ModelService); the rest are fallback
	// models from X-Gateway-Fallback-Models, deduplicated in declared order
	// and kept after passing catalog + subscription validation. Length = 1
	// when no fallback is declared. The M7 outer loop iterates this sequence
	// directly instead of redoing M5.
	ModelChain []*ModelService

	// === Written by M7 (the model that actually succeeded) ===
	//
	// RoutedModelService != ModelService on cross-model fallback.
	// RoutedModelService == ModelService when there's no fallback.
	RoutedModelService *ModelService

	// === Written by M6 Limit ===
	// nil = no quota policy applies to this request.
	RateLimit *RateLimitState

	// === Written by M7 Schedule ===
	Endpoint *Endpoint

	// === Written during the response phase (inside M7 / by M10) ===
	Usage              *Usage
	Error              *AdapterError
	SchedulingDecision *SchedulingDecision

	// === Extension point ===
	//
	// **context.Context is not on RC**: the single source of truth is
	// `c.Request.Context()`, relayed by each middleware via
	// `c.Request = c.Request.WithContext(ctx)`. Hanging a ctx field on a
	// mutable struct violates Go's "context is values, not state" principle,
	// and would drift from gin's native `c.Request.Context()` (a span gets
	// lost if, after M1, only RC.Ctx is updated but not c.Request).
	Extras map[string]any // transient / experimental fields
}
