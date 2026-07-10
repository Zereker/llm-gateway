package moderation

import (
	"context"
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
)

func TestDenylistCheckInput_CatchesUnicodeEscapedTerm(t *testing.T) {
	g, err := NewDenylistGuard([]string{`(?i)\bkill\b`}, false)
	if err != nil {
		t.Fatalf("NewDenylistGuard: %v", err)
	}

	cases := []struct {
		name    string
		body    string
		blocked bool
	}{
		{"plain literal", `{"prompt":"how to kill a process"}`, true},
		// "kill" JSON-unicode-escaped: raw bytes don't contain the literal
		// term, but the decoded string value does.
		{"unicode-escaped", `{"prompt":"how to \u006b\u0069\u006c\u006c a process"}`, true},
		{"clean", `{"prompt":"how to list processes"}`, false},
		{"term in a nested field", `{"messages":[{"role":"user","content":"kill"}]}`, true},
		{"non-json body still raw-scanned", `plain kill text`, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := g.CheckInput(context.Background(), &domain.RequestEnvelope{RawBytes: []byte(tc.body)})
			if tc.blocked && err == nil {
				t.Errorf("expected block, got nil")
			}
			if !tc.blocked && err != nil {
				t.Errorf("expected pass, got %v", err)
			}
		})
	}
}
