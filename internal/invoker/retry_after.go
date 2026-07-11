package invoker

import (
	"net/http"
	"strconv"
	"time"
)

// parseRetryAfter extracts the upstream's "when will capacity come back" hint
// from a failed response's headers. Returns 0 when no usable hint exists.
//
// Recognized headers, in priority order:
//
//  1. Retry-After — RFC 9110: either delay-seconds ("30") or an HTTP-date.
//     Sent by OpenAI/Anthropic/Azure on 429 and by many upstreams on 503.
//  2. OpenAI style x-ratelimit-reset-requests / x-ratelimit-reset-tokens —
//     Go-duration-like values ("1s", "6m0s", "12ms"); the smaller wins
//     (the tighter bucket recovers first but both must clear to admit a call,
//     so the larger of the two present values is the honest wait — we take
//     the max across these two, then the min against Retry-After).
//  3. Anthropic style anthropic-ratelimit-requests-reset /
//     anthropic-ratelimit-tokens-reset — RFC 3339 timestamps; same max rule.
//
// The caller clamps the returned value (see selector cooldown), so this
// function only parses — it doesn't bound.
func parseRetryAfter(h http.Header, now time.Time) time.Duration {
	if d := parseRetryAfterValue(h.Get("Retry-After"), now); d > 0 {
		return d
	}

	// OpenAI-style duration pair: both buckets must clear → take the max.
	var openai time.Duration
	for _, k := range [...]string{"x-ratelimit-reset-requests", "x-ratelimit-reset-tokens"} {
		if d, err := time.ParseDuration(h.Get(k)); err == nil && d > openai {
			openai = d
		}
	}
	if openai > 0 {
		return openai
	}

	// Anthropic-style RFC 3339 pair: same max rule.
	var anthropic time.Duration
	for _, k := range [...]string{"anthropic-ratelimit-requests-reset", "anthropic-ratelimit-tokens-reset"} {
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
