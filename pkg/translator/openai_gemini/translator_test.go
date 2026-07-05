package openai_gemini

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// 成功响应正文恰好含 "error" 不应被误判成错误（旧的字节扫描 bug）；真错误 body 才是。
func TestIsGeminiError(t *testing.T) {
	success := []byte(`{"candidates":[{"content":{"parts":[{"text":"error"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":1,"totalTokenCount":9}}`)
	if isGeminiError(success) {
		t.Error("正文含 error 的成功响应被误判成错误")
	}
	errBody := []byte(`{"error":{"code":400,"message":"bad","status":"INVALID_ARGUMENT"}}`)
	if !isGeminiError(errBody) {
		t.Error("真错误响应未被识别")
	}
}

// 成功响应正文是 "error" 时应正常翻译 + 带 usage（不走 error passthrough）。
func TestResponseHandler_JSON_ContentIsError(t *testing.T) {
	h := openaiGemini{}.NewResponseHandler()
	body := `{"candidates":[{"content":{"parts":[{"text":"error"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":1,"totalTokenCount":9}}`
	_, _ = h.Feed([]byte(body))
	out, usage, err := h.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if !strings.Contains(string(out), `"object":"chat.completion"`) || !strings.Contains(string(out), `"content":"error"`) {
		t.Errorf("应翻译成 OpenAI shape, got: %s", out)
	}
	if usage == nil || usage.Total != 9 {
		t.Errorf("usage 应保留, got %+v", usage)
	}
}

// 安全拦截（无 candidates + promptFeedback.blockReason）：非流式 choices 非 null +
// content_filter。
func TestResponseHandler_JSON_SafetyBlock(t *testing.T) {
	h := openaiGemini{}.NewResponseHandler()
	body := `{"promptFeedback":{"blockReason":"SAFETY"}}`
	_, _ = h.Feed([]byte(body))
	out, _, err := h.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	choices := gjson.GetBytes(out, "choices")
	if !choices.IsArray() || len(choices.Array()) != 1 {
		t.Fatalf("choices 应是含一个元素的数组（非 null）, got: %s", out)
	}
	if choices.Array()[0].Get("finish_reason").String() != "content_filter" {
		t.Errorf("finish_reason 应 content_filter, got: %s", out)
	}
}

// 流式安全拦截：不是空流，发 content_filter 收尾 chunk。
func TestResponseHandler_SSE_SafetyBlock(t *testing.T) {
	h := openaiGemini{}.NewResponseHandler()
	chunk := "data: {\"promptFeedback\":{\"blockReason\":\"SAFETY\"}}\n\n"
	out, _ := h.Feed([]byte(chunk))
	final, _, _ := h.Flush()
	all := string(out) + string(final)
	if !strings.Contains(all, `"finish_reason":"content_filter"`) {
		t.Errorf("流式拦截应发 content_filter chunk, got: %s", all)
	}
	if !strings.Contains(all, "data: [DONE]") {
		t.Errorf("应有 [DONE], got: %s", all)
	}
}

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
