// Package anthropic_openai Anthropic 客户端 → OpenAI 上游的 Translator。
//
// 客户端按 Anthropic Messages 格式发请求；本 translator 翻成 OpenAI ChatCompletion
// 格式给 adapter 转发；上游响应再翻回 Anthropic 格式给客户端。
//
// **支持两种模式**：
//   - 非流式（stream=false）：buffer-then-translate；Flush 一次性翻整 body
//   - 流式（stream=true）：OpenAI SSE chunk 实时翻成 Anthropic SSE event 序列
//
// **v0.5 限制**：
//   - 只支持 chat（system + user/assistant + text content）
//   - 不支持 tool_use / vision / multi-block content
//   - **流式 message_start.usage.input_tokens 总是 0**——OpenAI 在末尾才发 usage chunk，
//     而 Anthropic 协议要求 message_start 立即给 input_tokens；妥协方案是先发 0，
//     真实 input_tokens 通过 rc.Usage 记账（计费层准确，客户端 SDK 看到 0）。
//     v0.6 可改成基于 prompt 长度的估算值。
//
// 字段映射详见 translateRequest / translateResponse / parseOpenAIChunk。
package anthropic_openai

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/translator"
	"github.com/zereker-labs/ai-gateway/pkg/usage/extractor"
)

type anthropicOpenAI struct{}

func (anthropicOpenAI) Source() domain.Protocol { return domain.ProtoAnthropic }
func (anthropicOpenAI) Target() domain.Protocol { return domain.ProtoOpenAI }

func (anthropicOpenAI) TranslateRequest(srcBody []byte) ([]byte, error) {
	return translateRequest(srcBody)
}

func (anthropicOpenAI) NewResponseHandler() translator.ResponseHandler {
	return &responseHandler{ex: extractor.NewOpenAI()}
}

// responseHandler 同时支持非流式（buffer）和流式（SSE event-by-event）。
//
// **usage 双轨**：
//   - rc.Usage（计费）：走 ex extractor.Session，Flush 返 ex.Final()
//   - 写客户端的 message_delta（翻译产物）：复用 outputTokens 字段，由翻译路径自己更新
//
// 之所以双轨：emitClosing 要把 output_tokens 当成 Anthropic SSE event 的字段写出去，
// 是 SSE 翻译的一部分；而 caller 拿的 Usage 是给计费 / 旁路记账的语义。两者都从同一份
// OpenAI usage chunk 来，但消费方向不同，分开维护更清晰。
type responseHandler struct {
	streamingDecided bool
	isStreaming      bool

	// 非流式状态
	bodyBuffer   []byte
	requestModel string

	// 流式状态
	sseBuffer                []byte // 跨 chunk 累计未完整事件
	messageID                string // 流式响应共享一个 msg_xxx id
	emittedMessageStart      bool
	emittedContentBlockStart bool
	emittedClosing           bool   // content_block_stop + message_delta + message_stop 是否已发
	upstreamModel            string // 从首个 chunk 取（OpenAI chunk.model）
	pendingStopReason        string // 从 finish_reason chunk 取，[DONE] 时随 message_delta 发
	outputTokens             int64  // SSE message_delta 用；翻译路径维护

	// usage 旁路（给 caller / 计费）
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
			h.messageID = "msg_" + randID()
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
		// 流式：补发收尾事件（如果上游意外 EOF 没发 [DONE]）
		var out bytes.Buffer
		h.emitClosing(&out)
		usage := h.ex.Final()
		if out.Len() == 0 {
			return nil, usage, nil
		}
		return out.Bytes(), usage, nil
	}

	// 非流式
	if len(h.bodyBuffer) == 0 {
		return nil, nil, nil
	}
	if isOpenAIError(h.bodyBuffer) {
		return h.bodyBuffer, nil, nil
	}
	body, err := translateResponse(h.bodyBuffer, h.requestModel)
	return body, h.ex.Final(), err
}

// detectStreaming 第一个 chunk 出现时按 prefix 判断流式与否。
func (h *responseHandler) detectStreaming(chunk []byte) {
	h.streamingDecided = true
	trimmed := bytes.TrimLeft(chunk, " \t\r\n")
	h.isStreaming = bytes.HasPrefix(trimmed, []byte("data:")) || bytes.HasPrefix(trimmed, []byte(":"))
}

// parseAndEmitStream 切出已完整的 OpenAI SSE chunk 并翻译成 Anthropic SSE event 序列。
//
// OpenAI SSE chunk 形态：
//
//	data: {"id":"chatcmpl-x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{...},"finish_reason":null}]}
//	data: {"id":"chatcmpl-x",...,"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}
//	data: [DONE]
//
// 翻译策略（详见 translateOpenAIChunk）：
//   - 首个 chunk → 发 message_start + content_block_start（input_tokens=0，限制见包注释）
//   - delta.role / delta.content → content_block_delta
//   - finish_reason → 暂存 pendingStopReason
//   - usage chunk → 记 inputTokens / outputTokens（用于 rc.Usage）
//   - [DONE] → 发 content_block_stop + message_delta + message_stop
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
		if bytes.Equal(dataPayload, []byte("[DONE]")) {
			h.emitClosing(&out)
			continue
		}
		h.translateOpenAIChunk(&out, dataPayload)
	}
}

// translateOpenAIChunk 解析单个 OpenAI chunk JSON 并发出对应 Anthropic 事件。
func (h *responseHandler) translateOpenAIChunk(out *bytes.Buffer, data []byte) {
	var ev struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Index int `json:"index"`
			Delta *struct {
				Role    string `json:"role,omitempty"`
				Content string `json:"content,omitempty"`
			} `json:"delta,omitempty"`
			FinishReason string `json:"finish_reason,omitempty"`
		} `json:"choices"`
		Usage *struct {
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage,omitempty"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return
	}
	if ev.Model != "" && h.upstreamModel == "" {
		h.upstreamModel = ev.Model
	}

	// 首个 chunk 发 message_start
	if !h.emittedMessageStart {
		h.emittedMessageStart = true
		writeEvent(out, "message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            h.messageID,
				"type":          "message",
				"role":          "assistant",
				"content":       []any{},
				"model":         h.upstreamModel,
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage": map[string]any{
					"input_tokens":  0, // 见包注释：OpenAI 末尾才发 usage
					"output_tokens": 1,
				},
			},
		})
	}

	for _, ch := range ev.Choices {
		// finish_reason 单独处理（也可能跟 delta 共存，但通常一个 chunk 只一个）
		if ch.FinishReason != "" {
			h.pendingStopReason = mapFinishReason(ch.FinishReason)
		}
		if ch.Delta == nil {
			continue
		}
		if ch.Delta.Content != "" {
			if !h.emittedContentBlockStart {
				h.emittedContentBlockStart = true
				writeEvent(out, "content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": 0,
					"content_block": map[string]any{
						"type": "text",
						"text": "",
					},
				})
			}
			writeEvent(out, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{
					"type": "text_delta",
					"text": ch.Delta.Content,
				},
			})
		}
		// delta.role 在 OpenAI 首 chunk 出现；Anthropic 不需要单独事件（已在 message_start 里）
	}

	if ev.Usage != nil {
		// 翻译路径只关心 outputTokens（要写进 message_delta event）；
		// inputTokens 走 extractor 旁路给 caller，本路径不需要。
		h.outputTokens = ev.Usage.CompletionTokens
	}
}

// emitClosing 发流式收尾三件套：content_block_stop + message_delta + message_stop。
//
// 幂等：emittedClosing 标志保证多次调用只发一次（[DONE] 提前到 + Flush 兜底都安全）。
func (h *responseHandler) emitClosing(out *bytes.Buffer) {
	if h.emittedClosing {
		return
	}
	if !h.emittedMessageStart {
		// 上游一字未回——message_start 都没发，直接跳过收尾
		return
	}
	h.emittedClosing = true

	if h.emittedContentBlockStart {
		writeEvent(out, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": 0,
		})
	}

	stopReason := h.pendingStopReason
	if stopReason == "" {
		stopReason = "end_turn"
	}
	delta := map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
	}
	if h.outputTokens > 0 {
		delta["usage"] = map[string]any{"output_tokens": h.outputTokens}
	}
	writeEvent(out, "message_delta", delta)

	writeEvent(out, "message_stop", map[string]any{
		"type": "message_stop",
	})
}

// writeEvent 写一个 SSE event：`event: <type>\ndata: <json>\n\n`。
func writeEvent(out *bytes.Buffer, eventType string, payload map[string]any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	out.WriteString("event: ")
	out.WriteString(eventType)
	out.WriteString("\ndata: ")
	out.Write(b)
	out.WriteString("\n\n")
}

// =============================================================================
// 非流式：Anthropic 输入端 / OpenAI 上游端 / Anthropic 输出端 shape + 翻译函数
// =============================================================================

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

type openAIRequest struct {
	Model         string          `json:"model"`
	Messages      []openAIMessage `json:"messages"`
	MaxTokens     *uint32         `json:"max_tokens,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	Stop          []string        `json:"stop,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	StreamOptions *openAIStreamOp `json:"stream_options,omitempty"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIStreamOp struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   *openAIUsage   `json:"usage,omitempty"`
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

// translateRequest Anthropic body → OpenAI body。
//
// 字段映射：
//
//	system               → 插入 messages[0] = {role:"system", content:system}
//	messages[role/content] → messages[role/content] (role 兼容)
//	max_tokens           → max_tokens
//	temperature/top_p    → 同名
//	stop_sequences       → stop（OpenAI 接受 []string）
//	stream               → stream（且 true 时注入 stream_options.include_usage=true）
func translateRequest(rawBody []byte) ([]byte, error) {
	var in anthropicRequest
	if err := json.Unmarshal(rawBody, &in); err != nil {
		return nil, fmt.Errorf("anthropic body parse: %w", err)
	}

	out := openAIRequest{
		Model:  in.Model,
		Stream: in.Stream,
	}
	if in.System != "" {
		out.Messages = append(out.Messages, openAIMessage{
			Role:    "system",
			Content: in.System,
		})
	}
	for _, m := range in.Messages {
		switch m.Role {
		case "user", "assistant":
			out.Messages = append(out.Messages, openAIMessage{
				Role:    m.Role,
				Content: m.Content,
			})
		default:
			return nil, fmt.Errorf("unsupported message role %q (handles user/assistant only; system 走 top-level system 字段)", m.Role)
		}
	}
	if in.MaxTokens > 0 {
		mt := in.MaxTokens
		out.MaxTokens = &mt
	}
	if in.Temperature != nil {
		out.Temperature = in.Temperature
	}
	if in.TopP != nil {
		out.TopP = in.TopP
	}
	if len(in.StopSequences) > 0 {
		out.Stop = in.StopSequences
	}
	if in.Stream {
		out.StreamOptions = &openAIStreamOp{IncludeUsage: true}
	}

	return json.Marshal(out)
}

// translateResponse OpenAI body → Anthropic body（非流式）。
//
// 返 *domain.Usage 给 caller 的职责已移出（走 extractor.NewOpenAI 旁路）；本函数
// 只把 OpenAI usage 翻进 Anthropic body 的 usage 字段（Anthropic 客户端期待的形态）。
func translateResponse(rawBody []byte, fallbackModel string) ([]byte, error) {
	var in openAIResponse
	if err := json.Unmarshal(rawBody, &in); err != nil {
		return nil, fmt.Errorf("openai response parse: %w", err)
	}

	model := in.Model
	if model == "" {
		model = fallbackModel
	}

	var content string
	var stopReason string
	if len(in.Choices) > 0 {
		content = in.Choices[0].Message.Content
		stopReason = mapFinishReason(in.Choices[0].FinishReason)
	} else {
		stopReason = "end_turn"
	}

	out := anthropicResponse{
		ID:    openAIIDOrGen(in.ID),
		Type:  "message",
		Role:  "assistant",
		Model: model,
		Content: []anthropicContentBlock{
			{Type: "text", Text: content},
		},
		StopReason: stopReason,
	}

	if in.Usage != nil {
		out.Usage = &anthropicUsage{
			InputTokens:  in.Usage.PromptTokens,
			OutputTokens: in.Usage.CompletionTokens,
		}
	}

	body, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("anthropic response marshal: %w", err)
	}
	return body, nil
}

// mapFinishReason OpenAI finish_reason → Anthropic stop_reason。
func mapFinishReason(r string) string {
	switch r {
	case "stop", "":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "stop_sequence"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// openAIIDOrGen OpenAI 的 id 形如 "chatcmpl-xxx"；Anthropic 客户端期望 "msg_xxx"。
func openAIIDOrGen(openaiID string) string {
	if openaiID == "" {
		return "msg_" + randID()
	}
	return "msg_" + strings.TrimPrefix(openaiID, "chatcmpl-")
}

// isOpenAIError 粗判 body 是否错误响应（top-level "error" key）。
func isOpenAIError(body []byte) bool {
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
	translator.Register(anthropicOpenAI{})
}
