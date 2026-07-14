package middleware

// Client-facing X-Gateway-* request header constants.
//
// These headers let clients override gateway default behavior on a per-request basis:
//
//	X-Gateway-Timeout:           per-request timeout (duration string, e.g. "30s"); can only be stricter than cfg.timeout
//	X-Gateway-Max-Attempts:      M7 cross-endpoint retry cap (int); can only be smaller than cfg.scheduler.max_attempts
//	X-Gateway-Fallback-Models:   L3 cross-model fallback sequence (comma-separated model names); when all
//	                             endpoints for the current model fail, retries with the next model in the list. Empty = L3 disabled.
//	X-Gateway-Region:            optional region preference evaluated by virtual-model policy; cannot bypass subscriptions or allow/deny rules.
//	X-Gateway-Session:           session affinity key; requests with the same session stick to the same
//	                             upstream endpoint (for prefix/KV cache hits). Only takes effect when scoring/affinity
//	                             is enabled; empty = no stickiness.
//
// All headers silently fall back to cfg defaults on parse failure; a malformed
// header never blocks a request.
//
// Naming convention: all gateway custom headers use the X-Gateway-* prefix, to
// distinguish them from vendor / client headers.
const (
	HeaderGatewayTimeout        = "X-Gateway-Timeout"
	HeaderGatewayMaxAttempts    = "X-Gateway-Max-Attempts"
	HeaderGatewayFallbackModels = "X-Gateway-Fallback-Models"
	HeaderGatewayRegion         = "X-Gateway-Region"
	HeaderGatewaySession        = "X-Gateway-Session"
	// X-Gateway-Cache (request): off = don't read/write the cache for this
	// request; on = force caching (even if temperature≠0, client accepts the
	// non-determinism risk). Default: only caches deterministic requests
	// (non-streaming + temperature=0).
	// X-Gateway-Cache (response): hit = this response came from the cache.
	HeaderGatewayCache = "X-Gateway-Cache"
)
