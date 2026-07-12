package openai_cohere

import (
	"github.com/tidwall/gjson"
	"strings"
	"testing"
)

func TestCohereStreamTranslate(t *testing.T) {
	h := &responseHandler{}
	// simulate Cohere v2 SSE, fed in two chunks (including a partial line spanning Feed calls)
	out1, _ := h.Feed([]byte("data: {\"type\":\"message-start\"}\n\ndata: {\"type\":\"content-delta\",\"delta\":{\"message\":{\"content\":{\"text\":\"Hel\"}}}}\n\ndata: {\"type\":\"content-de"))
	out2, _ := h.Feed([]byte("lta\",\"delta\":{\"message\":{\"content\":{\"text\":\"lo\"}}}}\n\ndata: {\"type\":\"message-end\",\"delta\":{\"finish_reason\":\"COMPLETE\",\"usage\":{\"billed_units\":{\"input_tokens\":2,\"output_tokens\":2},\"tokens\":{\"input_tokens\":3,\"output_tokens\":2}}}}\n\n"))
	fin, usage, _ := h.Flush()
	all := string(out1) + string(out2) + string(fin)
	// the assembled text
	var text strings.Builder
	for _, line := range strings.Split(all, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		text.WriteString(gjson.Get(line[6:], "choices.0.delta.content").String())
	}
	if text.String() != "Hello" {
		t.Errorf("streamed text=%q want Hello", text.String())
	}
	if !strings.Contains(all, "[DONE]") {
		t.Error("missing [DONE]")
	}
	if !strings.Contains(all, `"finish_reason":"stop"`) {
		t.Error("missing finish_reason stop")
	}
	if usage == nil || usage.Total != 5 {
		t.Errorf("usage=%+v want total 5", usage)
	}
	// streaming Raw must also preserve billed_units verbatim (regression: the
	// streaming path never set Raw at all).
	if !gjson.GetBytes(usage.Raw, "billed_units").Exists() {
		t.Errorf("billed_units dropped from streaming Raw: %s", usage.Raw)
	}
}
