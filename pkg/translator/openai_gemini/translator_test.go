package openai_gemini

import (
	"strings"
	"testing"
)

// 流式：Gemini SSE chunk → OpenAI chat.completion.chunk SSE + usage 从末帧抽。
func TestResponseHandler_SSE(t *testing.T) {
	h := openaiGemini{}.NewResponseHandler()

	// 两个 Gemini SSE chunk：首帧内容，末帧内容 + finishReason + usageMetadata。
	chunk1 := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello\"}],\"role\":\"model\"},\"index\":0}]}\n\n"
	chunk2 := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\" world\"}],\"role\":\"model\"},\"finishReason\":\"STOP\",\"index\":0}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":2,\"totalTokenCount\":7}}\n\n"

	out1, err := h.Feed([]byte(chunk1))
	if err != nil {
		t.Fatalf("Feed1: %v", err)
	}
	out2, err := h.Feed([]byte(chunk2))
	if err != nil {
		t.Fatalf("Feed2: %v", err)
	}
	final, usage, err := h.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	all := string(out1) + string(out2) + string(final)

	for _, want := range []string{
		`"object":"chat.completion.chunk"`,
		`"role":"assistant"`,
		`"content":"Hello"`,
		`"content":" world"`,
		`"finish_reason":"stop"`,
		"data: [DONE]",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("SSE 输出缺 %q\n全文:\n%s", want, all)
		}
	}
	// role delta 只发一次。
	if n := strings.Count(all, `"role":"assistant"`); n != 1 {
		t.Errorf("role delta 应只发一次, got %d", n)
	}
	if usage == nil || usage.Input != 5 || usage.Output != 2 || usage.Total != 7 {
		t.Errorf("usage=%+v, want in=5 out=2 total=7", usage)
	}
}

// 跨 Feed 边界：半行留在 lineBuf，下次 Feed 拼回。
func TestResponseHandler_SSE_SplitAcrossFeeds(t *testing.T) {
	h := openaiGemini{}.NewResponseHandler()
	full := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hi\"}]},\"index\":0}]}\n\n"
	// 在中间切开
	mid := len(full) / 2
	_, _ = h.Feed([]byte(full[:mid]))
	out, _ := h.Feed([]byte(full[mid:]))
	final, _, _ := h.Flush()
	all := string(out) + string(final)
	if !strings.Contains(all, `"content":"Hi"`) {
		t.Errorf("跨 Feed 边界内容丢失, got: %s", all)
	}
}

// 非流式 JSON 路径仍正常（buffer-then-translate）。
func TestResponseHandler_JSON(t *testing.T) {
	h := openaiGemini{}.NewResponseHandler()
	body := `{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}}`
	if _, err := h.Feed([]byte(body)); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	out, usage, err := h.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"object":"chat.completion"`) || !strings.Contains(s, `"content":"answer"`) {
		t.Errorf("JSON 翻译输出异常: %s", s)
	}
	if strings.Contains(s, "chunk") {
		t.Errorf("非流式不应是 chunk shape: %s", s)
	}
	if usage == nil || usage.Total != 4 {
		t.Errorf("usage=%+v, want total=4", usage)
	}
}
