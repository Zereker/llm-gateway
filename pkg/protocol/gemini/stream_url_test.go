package gemini

import (
	"context"
	"strings"
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
)

func TestGeminiStreamURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{
			"https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-flash:generateContent",
			"https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-flash:streamGenerateContent?alt=sse",
		},
		// 已带 query（如 ?key=）→ 保留 + 追加 alt=sse
		{
			"https://x/models/m:generateContent?key=abc",
			"https://x/models/m:streamGenerateContent?key=abc&alt=sse",
		},
		// 已经是流式端点 → 只补 alt=sse
		{
			"https://x/models/m:streamGenerateContent",
			"https://x/models/m:streamGenerateContent?alt=sse",
		},
		// 已带 alt=sse → 不重复
		{
			"https://x/models/m:streamGenerateContent?alt=sse",
			"https://x/models/m:streamGenerateContent?alt=sse",
		},
		// 非标准 URL（找不到 :generateContent）→ 原样
		{"https://custom/gateway/path", "https://custom/gateway/path"},
	}
	for _, c := range cases {
		if got := geminiStreamURL(c.in); got != c.want {
			t.Errorf("geminiStreamURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// streaming session 应把 URL 换成 streamGenerateContent；非流式保持原样。
func TestSession_BuildRequest_StreamingURL(t *testing.T) {
	ep := &domain.Endpoint{Routing: domain.RoutingConfig{URL: "https://x/models/m:generateContent"}}
	tp := fakeTokenProvider{hdrName: "x-goog-api-key", hdrValue: "k"}

	s := newSession(context.Background(), ep, tp, true)
	req, err := s.BuildRequest([]byte(`{}`), nil)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if !strings.Contains(req.URL.String(), ":streamGenerateContent") || !strings.Contains(req.URL.RawQuery, "alt=sse") {
		t.Errorf("streaming URL = %q", req.URL.String())
	}

	ns := newSession(context.Background(), ep, tp, false)
	nreq, _ := ns.BuildRequest([]byte(`{}`), nil)
	if strings.Contains(nreq.URL.String(), "streamGenerateContent") {
		t.Errorf("non-streaming should keep :generateContent, got %q", nreq.URL.String())
	}
}
