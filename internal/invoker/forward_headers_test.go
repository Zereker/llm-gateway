package invoker

import (
	"net/http"
	"testing"
)

func TestCopyHeaders_StripsSensitiveAndForwardsSafe(t *testing.T) {
	src := http.Header{}
	src.Set("Content-Type", "text/event-stream")
	src.Set("Cache-Control", "no-cache")
	src.Add("Set-Cookie", "session=abc")
	src.Set("Content-Length", "123")
	src.Set("Openai-Organization", "org-secret")
	src.Set("X-Ratelimit-Remaining-Requests", "42")
	src.Set("Connection", "keep-alive")
	src.Set("X-Request-Id", "req-123")

	dst := http.Header{}
	copyHeaders(dst, src)

	// forwarded
	if dst.Get("Content-Type") != "text/event-stream" {
		t.Error("Content-Type should be forwarded (SSE needs it)")
	}
	if dst.Get("Cache-Control") != "no-cache" {
		t.Error("Cache-Control should be forwarded")
	}
	if dst.Get("X-Request-Id") != "req-123" {
		t.Error("X-Request-Id should be forwarded")
	}

	// stripped
	for _, h := range []string{
		"Set-Cookie", "Content-Length", "Openai-Organization",
		"X-Ratelimit-Remaining-Requests", "Connection",
	} {
		if dst.Get(h) != "" {
			t.Errorf("%s must not be forwarded to the client, got %q", h, dst.Get(h))
		}
	}
}

func TestRefuseRedirect(t *testing.T) {
	if err := refuseRedirect(nil, nil); err == nil {
		t.Fatal("CheckRedirect must return an error to refuse following redirects")
	}
	// the default client must carry the guard
	c := defaultHTTPClient()
	if c.CheckRedirect == nil {
		t.Fatal("default upstream client must set CheckRedirect")
	}
	if err := c.CheckRedirect(nil, nil); err == nil {
		t.Error("default client's CheckRedirect must refuse redirects")
	}
}
