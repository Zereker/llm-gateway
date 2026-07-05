// Package openai_cohere OpenAI 客户端 → Cohere v2 上游的 Translator。
//
// Cohere v2 /v2/chat 的请求跟 OpenAI 很接近（messages: role+content），但**响应
// shape 不同**：message.content 是数组([{type:"text",text}])、usage 嵌在
// usage.tokens.{input,output}_tokens、finish_reason 是大写枚举。本 translator 把
// 请求翻过去、把响应翻回 OpenAI ChatCompletion 给客户端。
//
// **流式**：客户端 stream:true 时 Cohere 返回带 type 的 SSE 事件流,responseHandler
// 增量翻成 OpenAI SSE chunk（见下）。非流式则 buffer-then-translate。**限制**：只支持
// chat 文本;不支持 tool/vision。
//
// 其它客户端协议（Anthropic / Responses）经 OpenAI pivot 组合到达（见 translator.compose）。
package openai_cohere

import (
	"bytes"
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
	Stream      bool            `json:"stream,omitempty"`
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
		Stream:      in.Stream, // 透传客户端 stream 标志
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

// responseHandler 自适应上游格式:
//   - JSON(非流式):buffer-then-translate,Flush 一次性翻成 OpenAI ChatCompletion。
//   - SSE(流式,客户端 stream:true):增量把 Cohere v2 事件翻成 OpenAI SSE chunk。
//
// 首个 chunk 按第一个非空字节嗅探:'{' → JSON;否则 → SSE。
type respMode int

const (
	modeUnknown respMode = iota
	modeJSON
	modeSSE
)

type responseHandler struct {
	mode    respMode
	buf     []byte // JSON 模式累积 / 未定模式暂存
	lineBuf []byte // SSE 模式的行缓冲（跨 Feed 保留半行）
	id      string
	usage   *domain.Usage
}

func (h *responseHandler) Feed(chunk []byte) ([]byte, error) {
	switch h.mode {
	case modeJSON:
		h.buf = append(h.buf, chunk...)
		return nil, nil
	case modeSSE:
		h.lineBuf = append(h.lineBuf, chunk...)
		return h.drainSSE(), nil
	default: // 未定：嗅探
		h.buf = append(h.buf, chunk...)
		t := bytes.TrimLeft(h.buf, " \t\r\n")
		if len(t) == 0 {
			return nil, nil // 还没拿到非空字节
		}
		if t[0] == '{' {
			h.mode = modeJSON
			return nil, nil
		}
		h.mode = modeSSE
		h.lineBuf = h.buf
		h.buf = nil
		return h.drainSSE(), nil
	}
}

func (h *responseHandler) Flush() ([]byte, *domain.Usage, error) {
	switch h.mode {
	case modeSSE:
		out := h.drainSSE()
		out = append(out, "data: [DONE]\n\n"...) // OpenAI 流终止符
		return out, h.usage, nil
	default: // JSON / 空
		if len(h.buf) == 0 {
			return nil, nil, nil
		}
		// error path：成功响应 message 是对象；错误(message 字符串/缺)原样透传保 visibility。
		if !gjson.GetBytes(h.buf, "message").IsObject() {
			return h.buf, nil, nil
		}
		body, usage := translateResponse(h.buf)
		return body, usage, nil
	}
}

// drainSSE 从 lineBuf 抽出完整行,把 Cohere `data:` 事件翻成 OpenAI SSE chunk。
func (h *responseHandler) drainSSE() []byte {
	var out []byte
	for {
		i := bytes.IndexByte(h.lineBuf, '\n')
		if i < 0 {
			break // 半行,留到下次
		}
		line := bytes.TrimRight(h.lineBuf[:i], "\r")
		h.lineBuf = h.lineBuf[i+1:]
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if len(data) == 0 {
			continue
		}
		out = append(out, h.translateEvent(data)...)
	}
	return out
}

// translateEvent 单个 Cohere v2 流事件 → OpenAI SSE chunk。
func (h *responseHandler) translateEvent(data []byte) []byte {
	ev := gjson.ParseBytes(data)
	switch ev.Get("type").String() {
	case "message-start":
		return h.chunk(map[string]any{"role": "assistant"}, "")
	case "content-delta":
		text := ev.Get("delta.message.content.text").String()
		if text == "" {
			return nil
		}
		return h.chunk(map[string]any{"content": text}, "")
	case "message-end":
		in := ev.Get("delta.usage.tokens.input_tokens").Int()
		outTok := ev.Get("delta.usage.tokens.output_tokens").Int()
		h.usage = &domain.Usage{Input: in, Output: outTok, Total: in + outTok, Source: domain.UsageSourceExtracted}
		return h.chunk(map[string]any{}, mapFinishReason(ev.Get("delta.finish_reason").String()))
	default: // content-start / content-end 等：跳过
		return nil
	}
}

// chunk 构造一个 OpenAI chat.completion.chunk 的 SSE 行。
func (h *responseHandler) chunk(delta map[string]any, finish string) []byte {
	if h.id == "" {
		h.id = "chatcmpl-" + randHex(8)
	}
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": nil}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	b, _ := json.Marshal(map[string]any{
		"id": h.id, "object": "chat.completion.chunk", "choices": []any{choice},
	})
	return append(append([]byte("data: "), b...), '\n', '\n')
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
