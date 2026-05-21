// Package openai_anthropic OpenAI 客户端 → Anthropic 上游的 Translator。
//
// 客户端按 OpenAI ChatCompletion 格式发请求；本 translator 翻成 Anthropic Messages
// 格式给 adapter 转发；上游响应再翻回 OpenAI 格式。
//
// **支持两种模式**：
//   - 非流式（stream=false）：buffer-then-translate；Flush 一次性翻整 body
//   - 流式（stream=true）：SSE event-by-event 实时翻译，客户端拿到 OpenAI SSE 体验
//
// **v0.5 限制**：
//   - 只支持 chat（system/user/assistant + text content）
//   - 不支持 tool_use / vision / multi-block content
//
// 字段映射详见 translateRequest / translateResponse / parseAndEmitStreamEvent。
package openai_anthropic

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/translator"
	"github.com/zereker/llm-gateway/pkg/usage/extractor"
)

// Anthropic Messages API 必填 max_tokens；client 没传时用这个 default。
// 4096 是个保守值（claude-3-5-sonnet 上限 8192；haiku 上限 8192）。
const defaultAnthropicMaxTokens uint32 = 4096

type openaiAnthropic struct{}

// Translator (OpenAI → Anthropic) 公共构造器——给 pkg/protocol/anthropic 用。
func Translator() translator.Translator { return openaiAnthropic{} }

func (openaiAnthropic) Source() domain.Protocol { return domain.ProtoOpenAI }
func (openaiAnthropic) Target() domain.Protocol { return domain.ProtoAnthropic }

func (openaiAnthropic) TranslateRequest(srcBody []byte) ([]byte, error) {
	return translateRequest(srcBody)
}

func (openaiAnthropic) NewResponseHandler() translator.ResponseHandler {
	return &responseHandler{ex: extractor.NewAnthropic()}
}

// responseHandler 同时支持非流式（buffer）和流式（SSE event-by-event）。
//
// 启动时不知道是哪种模式；第一个 chunk 到来时按 prefix 检测：
//   - "event:" / "data:" / ":" 开头 → SSE 流式
//   - 否则 → 非流式 buffer
//
// usage 提取走 extractor.Anthropic 旁路（v0.5 G6 抽出）：每次 Feed 把 chunk 同时
// 喂给 ex；Flush 直接拿 ex.Final()。本 handler 主路径只关心 chunk 翻译。
type responseHandler struct {
	streamingDecided bool
	isStreaming      bool

	// 非流式状态
	bodyBuffer   []byte
	requestModel string

	// 流式状态
	sseBuffer     []byte // 跨 chunk 累计未完整事件
	chatcmplID    string // 流式响应共享一个 id（OpenAI 约定 chunks 共享 id）
	upstreamModel string // 从 message_start.message.model 取
	createdSec    int64  // 流式 chunks 共享 created 秒（首个 chunk 时确定）

	// usage 旁路
	ex extractor.Session
}

func (h *responseHandler) Feed(chunk []byte) ([]byte, error) {
	if len(chunk) == 0 {
		return nil, nil
	}
	h.ex.Feed(chunk)
	if !h.streamingDecided {
		h.detectStreaming(chunk)
		if h.isStreaming {
			h.chatcmplID = "chatcmpl-" + randID()
			h.createdSec = time.Now().Unix()
		}
	}
	if h.isStreaming {
		h.sseBuffer = append(h.sseBuffer, chunk...)
		return h.parseAndEmitStream(), nil
	}
	h.bodyBuffer = append(h.bodyBuffer, chunk...)
	return nil, nil
}

func (h *responseHandler) Flush() ([]byte, *domain.Usage, error) {
	if h.isStreaming {
		return nil, h.ex.Final(), nil
	}

	// 非流式
	if len(h.bodyBuffer) == 0 {
		return nil, nil, nil
	}
	if isAnthropicError(h.bodyBuffer) {
		return h.bodyBuffer, nil, nil
	}
	body, err := translateResponse(h.bodyBuffer, h.requestModel)
	return body, h.ex.Final(), err
}

// detectStreaming 第一个 chunk 出现时判断流式与否。
func (h *responseHandler) detectStreaming(chunk []byte) {
	h.streamingDecided = true
	trimmed := bytes.TrimLeft(chunk, " \t\r\n")
	h.isStreaming = bytes.HasPrefix(trimmed, []byte("event:")) ||
		bytes.HasPrefix(trimmed, []byte("data:")) ||
		bytes.HasPrefix(trimmed, []byte(":"))
}

// parseAndEmitStream 切出已完整的 SSE event 并翻译成 OpenAI SSE chunks。
//
// 返回字节直接写客户端（已含 "data: " prefix + "\n\n" 分隔符）。
func (h *responseHandler) parseAndEmitStream() []byte {
	var out bytes.Buffer
	for {
		idx := bytes.Index(h.sseBuffer, []byte("\n\n"))
		if idx < 0 {
			return out.Bytes()
		}
		event := h.sseBuffer[:idx]
		h.sseBuffer = h.sseBuffer[idx+2:]

		// 提取 data: 行
		var dataPayload []byte
		for _, line := range bytes.Split(event, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) {
				dataPayload = bytes.TrimSpace(line[5:])
				break
			}
		}
		if dataPayload == nil {
			continue
		}
		h.translateAnthropicEvent(&out, dataPayload)
	}
}

// translateAnthropicEvent 翻译单个 Anthropic SSE event payload → 写 OpenAI SSE chunk(s) 到 out。
//
// Anthropic event types → OpenAI chunk 映射：
//
//	message_start       → 1 个 chunk: delta={role:"assistant"}（OpenAI SDK 期望先收到 role）
//	content_block_start → 不输出（无文本增量）
//	content_block_delta → 1 个 chunk: delta={content:"<text>"}
//	content_block_stop  → 不输出
//	message_delta       → 1 个 chunk: delta={}, finish_reason=<mapped>（携带 stop_reason 时）
//	message_stop        → 1 个 sentinel: data: [DONE]
//	ping                → 不输出
//
// usage 提取**不在这里**——extractor.Anthropic 在 Feed 时已经独立扫一遍。本函数只
// 关心翻译，不读不写 token 字段。
func (h *responseHandler) translateAnthropicEvent(out *bytes.Buffer, data []byte) {
	var ev struct {
		Type    string `json:"type"`
		Message *struct {
			ID    string `json:"id"`
			Model string `json:"model"`
		} `json:"message,omitempty"`
		Index int `json:"index"`
		Delta *struct {
			Type       string `json:"type"`
			Text       string `json:"text,omitempty"`
			StopReason string `json:"stop_reason,omitempty"`
		} `json:"delta,omitempty"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return
	}

	switch ev.Type {
	case "message_start":
		if ev.Message != nil && ev.Message.Model != "" {
			h.upstreamModel = ev.Message.Model
		}
		// 输出 role chunk（OpenAI SDK 习惯）
		h.writeChunk(out, map[string]any{"role": "assistant"}, "")

	case "content_block_delta":
		if ev.Delta != nil && ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
			h.writeChunk(out, map[string]any{"content": ev.Delta.Text}, "")
		}

	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			h.writeChunk(out, map[string]any{}, mapStopReason(ev.Delta.StopReason))
		}

	case "message_stop":
		out.WriteString("data: [DONE]\n\n")

	case "ping", "content_block_start", "content_block_stop":
		// 无输出
	}
}

// writeChunk 拼一个 OpenAI SSE chunk 写到 out（含 "data: " prefix + "\n\n" 分隔符）。
//
// finishReason 空时写 null；非空时写字符串。
func (h *responseHandler) writeChunk(out *bytes.Buffer, delta map[string]any, finishReason string) {
	choice := map[string]any{
		"index":         0,
		"delta":         delta,
		"finish_reason": nil,
	}
	if finishReason != "" {
		choice["finish_reason"] = finishReason
	}
	chunk := map[string]any{
		"id":      h.chatcmplID,
		"object":  "chat.completion.chunk",
		"created": h.createdSec,
		"model":   h.upstreamModel,
		"choices": []map[string]any{choice},
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	out.WriteString("data: ")
	out.Write(b)
	out.WriteString("\n\n")
}

// =============================================================================
// 非流式：OpenAI 输入端 / Anthropic 上游端 / OpenAI 输出端 shape + 翻译函数
// =============================================================================

type openAIRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	MaxTokens   *uint32         `json:"max_tokens,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Stop        json.RawMessage `json:"stop,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicRequest struct {
	Model         string             `json:"model"`
	Messages      []anthropicMessage `json:"messages"`
	System        string             `json:"system,omitempty"`
	MaxTokens     uint32             `json:"max_tokens"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	ID         string                  `json:"id"`
	Type       string                  `json:"type"`
	Role       string                  `json:"role"`
	Content    []anthropicContentBlock `json:"content"`
	Model      string                  `json:"model"`
	StopReason string                  `json:"stop_reason"`
	Usage      *anthropicUsage         `json:"usage,omitempty"`
}

type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type openAIResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   openAIUsage    `json:"usage"`
}

type openAIChoice struct {
	Index        int           `json:"index"`
	Message      openAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type openAIUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// translateRequest OpenAI body → Anthropic body。**保留 stream 字段透传**给上游。
func translateRequest(rawBody []byte) ([]byte, error) {
	var in openAIRequest
	if err := json.Unmarshal(rawBody, &in); err != nil {
		return nil, fmt.Errorf("openai body parse: %w", err)
	}

	out := anthropicRequest{
		Model:  in.Model,
		Stream: in.Stream,
	}

	var systemParts []string
	for _, m := range in.Messages {
		switch m.Role {
		case "system":
			if m.Content != "" {
				systemParts = append(systemParts, m.Content)
			}
		case "user", "assistant":
			out.Messages = append(out.Messages, anthropicMessage{
				Role:    m.Role,
				Content: m.Content,
			})
		default:
			return nil, fmt.Errorf("unsupported message role %q (handles system/user/assistant only)", m.Role)
		}
	}
	if len(systemParts) > 0 {
		out.System = strings.Join(systemParts, "\n\n")
	}

	if in.MaxTokens != nil && *in.MaxTokens > 0 {
		out.MaxTokens = *in.MaxTokens
	} else {
		out.MaxTokens = defaultAnthropicMaxTokens
	}
	if in.Temperature != nil {
		out.Temperature = in.Temperature
	}
	if in.TopP != nil {
		out.TopP = in.TopP
	}
	if len(in.Stop) > 0 {
		out.StopSequences = parseStopField(in.Stop)
	}

	return json.Marshal(out)
}

func parseStopField(raw json.RawMessage) []string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []string{s}
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr
	}
	return nil
}

// translateResponse Anthropic body → OpenAI body（非流式）。
//
// 返 *domain.Usage 给 caller 的职责已移出（走 extractor.NewAnthropic 旁路）；本函数
// 只把 Anthropic usage 翻进 OpenAI body 的 usage 字段（OpenAI 客户端期待的形态）。
func translateResponse(rawBody []byte, fallbackModel string) ([]byte, error) {
	var in anthropicResponse
	if err := json.Unmarshal(rawBody, &in); err != nil {
		return nil, fmt.Errorf("anthropic response parse: %w", err)
	}

	model := in.Model
	if model == "" {
		model = fallbackModel
	}

	var content strings.Builder
	for _, blk := range in.Content {
		if blk.Type == "text" {
			content.WriteString(blk.Text)
		}
	}

	out := openAIResponse{
		ID:      anthropicIDOrGen(in.ID),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []openAIChoice{
			{
				Index:        0,
				Message:      openAIMessage{Role: "assistant", Content: content.String()},
				FinishReason: mapStopReason(in.StopReason),
			},
		},
	}

	if in.Usage != nil {
		out.Usage = openAIUsage{
			PromptTokens:     in.Usage.InputTokens,
			CompletionTokens: in.Usage.OutputTokens,
			TotalTokens:      in.Usage.InputTokens + in.Usage.OutputTokens,
		}
	}

	body, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("openai response marshal: %w", err)
	}
	return body, nil
}

// mapStopReason Anthropic stop_reason → OpenAI finish_reason。
func mapStopReason(r string) string {
	switch r {
	case "end_turn", "stop_sequence", "":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

// anthropicIDOrGen Anthropic 的 id 形如 "msg_xxx"；OpenAI 客户端期望 "chatcmpl-xxx"。
func anthropicIDOrGen(anthropicID string) string {
	if anthropicID == "" {
		return "chatcmpl-" + randID()
	}
	return "chatcmpl-" + strings.TrimPrefix(anthropicID, "msg_")
}

// isAnthropicError 粗判 body 是否错误响应（top-level "error" key）。
func isAnthropicError(body []byte) bool {
	for i := 0; i < len(body) && i < 30; i++ {
		if body[i] == '{' {
			rest := body[i:]
			if len(rest) > 200 {
				rest = rest[:200]
			}
			for j := 0; j+7 <= len(rest); j++ {
				if string(rest[j:j+7]) == `"error"` {
					return true
				}
			}
			return false
		}
	}
	return false
}

func randID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func init() {
	translator.Register(openaiAnthropic{})
}
