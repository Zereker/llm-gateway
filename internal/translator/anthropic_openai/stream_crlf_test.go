package anthropic_openai

import (
	"strings"
	"testing"
)

// A CRLF-framed OpenAI upstream stream must still be split into events and
// translated to Anthropic SSE — the shared NextSSEFrame scanner handles CRLF,
// so the client isn't left with a stalled (never-emitted) stream.
func TestResponseHandler_CRLFFramedStream(t *testing.T) {
	h := (anthropicOpenAI{}).NewResponseHandler()

	// two OpenAI chat chunks separated by a CRLF blank line
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
	}, "\r\n\r\n") + "\r\n\r\n"

	out, err := h.Feed([]byte(stream))
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("CRLF-framed stream produced no Anthropic output (frame boundary not detected)")
	}
	// the content delta should have surfaced as an Anthropic content_block_delta
	if !strings.Contains(string(out), "content_block_delta") && !strings.Contains(string(out), "hi") {
		t.Errorf("expected translated Anthropic events, got: %s", out)
	}
}
