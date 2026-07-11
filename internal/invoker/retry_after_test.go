package invoker

import (
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		headers map[string]string
		want    time.Duration
	}{
		{"no headers", nil, 0},
		{"retry-after seconds", map[string]string{"Retry-After": "30"}, 30 * time.Second},
		{"retry-after zero", map[string]string{"Retry-After": "0"}, 0},
		{"retry-after negative", map[string]string{"Retry-After": "-5"}, 0},
		{"retry-after http date", map[string]string{
			"Retry-After": now.Add(90 * time.Second).Format(http.TimeFormat),
		}, 90 * time.Second},
		{"retry-after past date", map[string]string{
			"Retry-After": now.Add(-time.Minute).Format(http.TimeFormat),
		}, 0},
		{"retry-after garbage", map[string]string{"Retry-After": "soon"}, 0},
		{"openai reset requests", map[string]string{
			"x-ratelimit-reset-requests": "1s",
		}, time.Second},
		{"openai reset both takes max", map[string]string{
			"x-ratelimit-reset-requests": "1s",
			"x-ratelimit-reset-tokens":   "6m0s",
		}, 6 * time.Minute},
		{"openai reset milliseconds", map[string]string{
			"x-ratelimit-reset-tokens": "12ms",
		}, 12 * time.Millisecond},
		{"retry-after wins over openai", map[string]string{
			"Retry-After":                "10",
			"x-ratelimit-reset-requests": "5m",
		}, 10 * time.Second},
		{"anthropic reset rfc3339", map[string]string{
			"anthropic-ratelimit-requests-reset": now.Add(45 * time.Second).Format(time.RFC3339),
		}, 45 * time.Second},
		{"anthropic reset both takes max", map[string]string{
			"anthropic-ratelimit-requests-reset": now.Add(10 * time.Second).Format(time.RFC3339),
			"anthropic-ratelimit-tokens-reset":   now.Add(2 * time.Minute).Format(time.RFC3339),
		}, 2 * time.Minute},
		{"anthropic reset in the past", map[string]string{
			"anthropic-ratelimit-requests-reset": now.Add(-time.Minute).Format(time.RFC3339),
		}, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tc.headers {
				h.Set(k, v)
			}
			if got := parseRetryAfter(h, now); got != tc.want {
				t.Errorf("parseRetryAfter = %v, want %v", got, tc.want)
			}
		})
	}
}
