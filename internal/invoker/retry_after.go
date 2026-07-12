package invoker

import (
	"net/http"
	"strconv"
	"time"
)

// parseRetryAfter extracts the upstream's "when will capacity come back" hint
// from a failed response's headers. Returns 0 when no usable hint exists.
//
// Recognized headers, in priority order (first family with a usable value
// wins — families are never mixed or compared against each other):
//
//  1. Retry-After — RFC 9110: either delay-seconds ("30") or an HTTP-date.
//     Sent by OpenAI/Anthropic/Azure on 429 and by many upstreams on 503.
//     Present and parseable → returned as-is, short-circuiting the rest.
//  2. OpenAI style x-ratelimit-reset-requests / x-ratelimit-reset-tokens —
//     Go-duration-like values ("1s", "6m0s", "12ms"); both buckets must
//     clear to admit a call, so the max of the pair is the honest wait.
//  3. Anthropic style anthropic-ratelimit-requests-reset /
//     anthropic-ratelimit-tokens-reset — RFC 3339 timestamps; same max rule.
//
// The caller clamps the returned value (see selector cooldown), so this
// function only parses — it doesn't bound.
// headerOpenAIRatelimitResetRequests and its sibling below name the two
// OpenAI-style rate-limit headers this file reads; factored out since the
// package's tests assert the parser reacts to these exact header names.
const (
	headerOpenAIRatelimitResetRequests = "x-ratelimit-reset-requests"
	headerOpenAIRatelimitResetTokens   = "x-ratelimit-reset-tokens"

	headerAnthropicRatelimitRequestsReset = "anthropic-ratelimit-requests-reset"
	headerAnthropicRatelimitTokensReset   = "anthropic-ratelimit-tokens-reset"
)

func parseRetryAfter(h http.Header, now time.Time) time.Duration {
	if d := parseRetryAfterValue(h.Get("Retry-After"), now); d > 0 {
		return d
	}

	// OpenAI-style duration pair: both buckets must clear → take the max.
	var openai time.Duration
	for _, k := range [...]string{headerOpenAIRatelimitResetRequests, headerOpenAIRatelimitResetTokens} {
		if d, err := time.ParseDuration(h.Get(k)); err == nil && d > openai {
			openai = d
		}
	}

	if openai > 0 {
		return openai
	}

	// Anthropic-style RFC 3339 pair: same max rule.
	var anthropic time.Duration
	for _, k := range [...]string{headerAnthropicRatelimitRequestsReset, headerAnthropicRatelimitTokensReset} {
		if t, err := time.Parse(time.RFC3339, h.Get(k)); err == nil {
			if d := t.Sub(now); d > anthropic {
				anthropic = d
			}
		}
	}

	return anthropic
}

// parseRetryAfterValue parses a single Retry-After value: delay-seconds or
// HTTP-date. Returns 0 on empty/unparseable/past values.
func parseRetryAfterValue(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}

	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}

		return time.Duration(secs) * time.Second
	}

	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}

	return 0
}
