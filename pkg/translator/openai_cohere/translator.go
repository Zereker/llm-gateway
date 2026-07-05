// Package openai_cohere OpenAI 客户端 → Cohere v2 上游的 Translator。
//
// Cohere v2 /v2/chat 的请求跟 OpenAI 很接近（messages: role+content），但**响应
// shape 不同**：message.content 是数组([{type:"text",text}])、usage 嵌在
// usage.tokens.{input,output}_tokens、finish_reason 是大写枚举。本 translator 把
// 请求翻过去、把响应翻回 OpenAI ChatCompletion 给客户端。
//
// **v1 限制**：只支持 chat 文本；非流式（buffer-then-translate at Flush；Cohere 流是
// 带 event type 的 SSE，跟 OpenAI 不同，另迭代）；不支持 tool/vision。
//
// 其它客户端协议（Anthropic / Responses）经 OpenAI pivot 组合到达（见 translator.compose）。
package openai_cohere

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/translator"
)

type openaiCohere struct{}

func (openaiCohere) Source() domain.Protocol { return domain.ProtoOpenAI }
func (openaiCohere) Target() domain.Protocol { return domain.ProtoCohere }

func (openaiCohere) TranslateRequest(srcBody []byte) ([]byte, error) {
	return translateRequest(srcBody)
}

func (openaiCohere) NewResponseHandler() translator.ResponseHandler {
	return &responseHandler{}
}

func init() { translator.Register(openaiCohere{}) }

// =============================================================================
// 请求：OpenAI ChatCompletion → Cohere v2 /v2/chat
// =============================================================================

type openaiReq struct {
	Model       string          `json:"model"`
	Messages    []openaiMsg     `json:"messages"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Stop        json.RawMessage `json:"stop,omitempty"`
}

type openaiMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type cohereReq struct {
	Model       string     `json:"model"`
	Messages    []cohereMsg `json:"messages"`
	MaxTokens   *int       `json:"max_tokens,omitempty"`
	Temperature *float64   `json:"temperature,omitempty"`
	P           *float64   `json:"p,omitempty"`
	Stream      bool       `json:"stream"`
}

type cohereMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func translateRequest(srcBody []byte) ([]byte, error) {
	var in openaiReq
	if err := json.Unmarshal(srcBody, &in); err != nil {
		return nil, err
	}
	out := cohereReq{
		Model:       in.Model,
		MaxTokens:   in.MaxTokens,
		Temperature: in.Temperature,
		P:           in.TopP,
		Stream:      false, // v1 非流式
	}
	out.Messages = make([]cohereMsg, 0, len(in.Messages))
	for _, m := range in.Messages {
		out.Messages = append(out.Messages, cohereMsg{Role: m.Role, Content: contentToString(m.Content)})
	}
	return json.Marshal(out)
}

// contentToString 把 OpenAI content（string 或 多模态数组）压成纯文本。
func contentToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	r := gjson.ParseBytes(raw)
	if r.Type == gjson.String {
		return r.String()
	}
	if r.IsArray() {
		var sb strings.Builder
		r.ForEach(func(_, part gjson.Result) bool {
			if t := part.Get("text"); t.Exists() {
				sb.WriteString(t.String())
			}
			return true
		})
		return sb.String()
	}
	return r.String()
}

// =============================================================================
// 响应：Cohere v2 → OpenAI ChatCompletion（buffer-then-translate）
// =============================================================================

type responseHandler struct {
	buf []byte
}

func (h *responseHandler) Feed(chunk []byte) ([]byte, error) {
	h.buf = append(h.buf, chunk...)
	return nil, nil // buffer 模式：不写客户端
}

func (h *responseHandler) Flush() ([]byte, *domain.Usage, error) {
	if len(h.buf) == 0 {
		return nil, nil, nil
	}
	// error path：Cohere 成功响应 message 是对象；错误响应 message 是字符串或缺 content。
	// 不翻译错误 body，原样透给客户端（保 error visibility，status 已是 upstream 的 4xx/5xx）。
	if !gjson.GetBytes(h.buf, "message").IsObject() {
		return h.buf, nil, nil
	}
	body, usage := translateResponse(h.buf)
	return body, usage, nil
}

func translateResponse(buf []byte) ([]byte, *domain.Usage) {
	root := gjson.ParseBytes(buf)

	// 拼 message.content[].text
	var text strings.Builder
	root.Get("message.content").ForEach(func(_, part gjson.Result) bool {
		if part.Get("type").String() == "text" {
			text.WriteString(part.Get("text").String())
		}
		return true
	})

	in := root.Get("usage.tokens.input_tokens").Int()
	outTok := root.Get("usage.tokens.output_tokens").Int()
	usage := &domain.Usage{
		Input:  in,
		Output: outTok,
		Total:  in + outTok,
		Source: domain.UsageSourceExtracted,
	}

	id := root.Get("id").String()
	if id == "" {
		id = "chatcmpl-" + randHex(8)
	}

	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text.String()},
			"finish_reason": mapFinishReason(root.Get("finish_reason").String()),
		}},
		"usage": map[string]any{
			"prompt_tokens":     in,
			"completion_tokens": outTok,
			"total_tokens":      in + outTok,
		},
	}
	body, _ := json.Marshal(resp)
	return body, usage
}

// mapFinishReason Cohere 大写枚举 → OpenAI。
func mapFinishReason(c string) string {
	switch c {
	case "COMPLETE", "STOP_SEQUENCE":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "":
		return "stop"
	default:
		return strings.ToLower(c)
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
