package openai_cohere

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/translator"

	// 让 anthropic→cohere 的 pivot 可达（组合需要 anthropic→openai 已注册）。
	_ "github.com/zereker/llm-gateway/pkg/translator/anthropic_openai"
)

func TestTranslateRequest(t *testing.T) {
	in := `{"model":"command-r","max_tokens":100,"temperature":0.3,"top_p":0.9,
	        "messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`
	out, err := translateRequest([]byte(in))
	if err != nil {
		t.Fatalf("translateRequest: %v", err)
	}
	r := gjson.ParseBytes(out)
	if r.Get("model").String() != "command-r" {
		t.Errorf("model = %q", r.Get("model").String())
	}
	if r.Get("max_tokens").Int() != 100 || r.Get("temperature").Float() != 0.3 || r.Get("p").Float() != 0.9 {
		t.Errorf("params 映射错: %s", out)
	}
	if r.Get("stream").Bool() != false {
		t.Error("stream 应 false")
	}
	if r.Get("messages.0.role").String() != "system" || r.Get("messages.1.content").String() != "hi" {
		t.Errorf("messages 映射错: %s", out)
	}
}

func TestTranslateRequest_MultimodalContentToText(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"part1 "},{"type":"text","text":"part2"}]}]}`
	out, _ := translateRequest([]byte(in))
	if c := gjson.GetBytes(out, "messages.0.content").String(); c != "part1 part2" {
		t.Errorf("content array 应压成文本, got %q", c)
	}
}

func TestTranslateResponse(t *testing.T) {
	cohere := `{"id":"c-123","finish_reason":"COMPLETE",
	           "message":{"role":"assistant","content":[{"type":"text","text":"Hello "},{"type":"text","text":"world"}]},
	           "usage":{"tokens":{"input_tokens":10,"output_tokens":5}}}`
	body, usage := translateResponse([]byte(cohere))
	r := gjson.ParseBytes(body)
	if r.Get("object").String() != "chat.completion" {
		t.Errorf("object = %q", r.Get("object").String())
	}
	if r.Get("choices.0.message.content").String() != "Hello world" {
		t.Errorf("content = %q", r.Get("choices.0.message.content").String())
	}
	if r.Get("choices.0.finish_reason").String() != "stop" {
		t.Errorf("finish_reason = %q, want stop", r.Get("choices.0.finish_reason").String())
	}
	if r.Get("usage.prompt_tokens").Int() != 10 || r.Get("usage.completion_tokens").Int() != 5 || r.Get("usage.total_tokens").Int() != 15 {
		t.Errorf("usage 映射错: %s", body)
	}
	if usage == nil || usage.Total != 15 || usage.Source != domain.UsageSourceExtracted {
		t.Errorf("usage struct = %+v", usage)
	}
}

// 上游 EOF 后的 handler Flush 完整走一遍。
func TestResponseHandler_BufferThenTranslate(t *testing.T) {
	h := &responseHandler{}
	// 分两段喂
	if b, _ := h.Feed([]byte(`{"id":"x","message":{"role":"assistant","content":[{"type":"text",`)); b != nil {
		t.Error("buffer 模式 Feed 不该返 bytes")
	}
	h.Feed([]byte(`"text":"ok"}]},"finish_reason":"COMPLETE","usage":{"tokens":{"input_tokens":1,"output_tokens":2}}}`))
	body, usage, err := h.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if gjson.GetBytes(body, "choices.0.message.content").String() != "ok" || usage.Total != 3 {
		t.Errorf("flush 结果错: body=%s usage=%+v", body, usage)
	}
}

// 错误响应（message 是字符串）原样透传，不翻译。
func TestResponseHandler_ErrorPassthrough(t *testing.T) {
	h := &responseHandler{}
	errBody := `{"id":"x","message":"invalid api token"}`
	h.Feed([]byte(errBody))
	body, usage, _ := h.Flush()
	if string(body) != errBody {
		t.Errorf("错误 body 应原样透传, got %s", body)
	}
	if usage != nil {
		t.Error("错误响应不该有 usage")
	}
}

func TestFinishReasonMap(t *testing.T) {
	for in, want := range map[string]string{"COMPLETE": "stop", "MAX_TOKENS": "length", "STOP_SEQUENCE": "stop", "": "stop"} {
		if got := mapFinishReason(in); got != want {
			t.Errorf("mapFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

// translator 注册 + 从各客户端协议可达（直连 openai / anthropic 经 pivot 组合）。
func TestCohereTranslatorReachable(t *testing.T) {
	if translator.Find(domain.ProtoOpenAI, domain.ProtoCohere) == nil {
		t.Fatal("openai→cohere translator 未注册")
	}
	if translator.FindVia(domain.ProtoAnthropic, domain.ProtoCohere, domain.ProtoOpenAI) == nil {
		t.Error("anthropic→cohere 经 pivot 应可达")
	}
}

// 确保 translateRequest 产出的是合法 JSON。
func TestTranslateRequest_ValidJSON(t *testing.T) {
	out, _ := translateRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if !json.Valid(out) {
		t.Errorf("非法 JSON: %s", out)
	}
}
