// Package openai_anthropic is the Translator for OpenAI clients → Anthropic upstream.
//
// Clients send requests in OpenAI ChatCompletion format; this translator converts
// them to Anthropic Messages format for the adapter to forward; upstream responses
// are translated back to OpenAI format.
//
// **Supports two modes**:
//   - Non-streaming (stream=false): buffer-then-translate; Flush translates the
//     whole body at once
//   - Streaming (stream=true): SSE event-by-event real-time translation, so the
//     client gets the OpenAI SSE experience
//
// **Limitations**:
//   - Only chat is supported (system/user/assistant messages)
//   - Multi-part content arrays are accepted, but only their text parts are
//     carried across; non-text parts (images / tool calls) are dropped
//
// See translateRequest / translateResponse / parseAndEmitStreamEvent for field mapping details.
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

// Anthropic Messages API requires max_tokens; used as the default when the
// client doesn't pass one. 4096 is a conservative value (claude-3-5-sonnet
// caps at 8192; haiku caps at 8192).
const defaultAnthropicMaxTokens uint32 = 4096

type openaiAnthropic struct{}

// New returns the OpenAI-to-Anthropic translator.
func New() translator.Translator { return openaiAnthropic{} }

func (openaiAnthropic) Source() domain.Protocol { return domain.ProtoOpenAI }
func (openaiAnthropic) Target() domain.Protocol { return domain.ProtoAnthropic }

func (openaiAnthropic) TranslateRequest(srcBody []byte) ([]byte, error) {
	translator.ReportLossyRequest(domain.ProtoOpenAI, domain.ProtoAnthropic, srcBody)
	return translateRequest(srcBody)
}

func (openaiAnthropic) NewResponseHandler() translator.ResponseHandler {
	return &responseHandler{ex: extractor.NewAnthropic()}
}

// responseHandler supports both non-streaming (buffer) and streaming
// (SSE event-by-event) modes.
//
// Which mode applies isn't known at start; it's detected by prefix when the
// first chunk arrives:
//   - starts with "event:" / "data:" / ":" → SSE streaming
//   - otherwise → non-streaming buffer
//
// Usage extraction goes through the extractor.Anthropic side channel (split
// out in v0.5 G6): each Feed call also feeds the chunk to ex; Flush just
// calls ex.Final(). This handler's main path only cares about chunk translation.
type responseHandler struct {
	streamingDecided bool
	isStreaming      bool

	// Non-streaming state
	bodyBuffer   []byte
	requestModel string

	// Streaming state
	sseBuffer     []byte // incomplete events accumulated across chunks
	chatcmplID    string // streaming response shares one id (OpenAI convention: chunks share an id)
	upstreamModel string // taken from message_start.message.model
	createdSec    int64  // streaming chunks share one created timestamp (fixed at first chunk)

	// usage side channel
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

	// Non-streaming
	if len(h.bodyBuffer) == 0 {
		return nil, nil, nil
	}
	if isAnthropicError(h.bodyBuffer) {
		return h.bodyBuffer, nil, nil
	}
	body, err := translateResponse(h.bodyBuffer, h.requestModel)
	return body, h.ex.Final(), err
}

// detectStreaming decides streaming vs. non-streaming when the first chunk arrives.
func (h *responseHandler) detectStreaming(chunk []byte) {
	h.streamingDecided = true
	trimmed := bytes.TrimLeft(chunk, " \t\r\n")
	h.isStreaming = bytes.HasPrefix(trimmed, []byte("event:")) ||
		bytes.HasPrefix(trimmed, []byte("data:")) ||
		bytes.HasPrefix(trimmed, []byte(":"))
}

// parseAndEmitStream slices out complete SSE events and translates them into
// OpenAI SSE chunks.
//
// The returned bytes are written to the client directly (already including
// the "data: " prefix + "\n\n" separator).
func (h *responseHandler) parseAndEmitStream() []byte {
	var out bytes.Buffer
	for {
		event, rest, ok := extractor.NextSSEFrame(h.sseBuffer)
		if !ok {
			return out.Bytes()
		}
		h.sseBuffer = rest

		// Extract the data: line
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

// translateAnthropicEvent translates a single Anthropic SSE event payload → writes
// OpenAI SSE chunk(s) to out.
//
// Anthropic event types → OpenAI chunk mapping:
//
//	message_start       → 1 chunk: delta={role:"assistant"} (OpenAI SDK expects role first)
//	content_block_start → no output (no text delta)
//	content_block_delta → 1 chunk: delta={content:"<text>"}
//	content_block_stop  → no output
//	message_delta       → 1 chunk: delta={}, finish_reason=<mapped> (when stop_reason is present)
//	message_stop        → 1 sentinel: data: [DONE]
//	ping                → no output
//
// Usage extraction does **not** happen here — extractor.Anthropic already scans
// independently in Feed. This function only cares about translation; it doesn't
// read or write token fields.
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
		// Emit the role chunk (OpenAI SDK convention)
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
		// No output
	}
}

// writeChunk assembles one OpenAI SSE chunk and writes it to out (including the
// "data: " prefix + "\n\n" separator).
//
// Writes null when finishReason is empty; writes the string otherwise.
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
// Non-streaming: OpenAI input side / Anthropic upstream side / OpenAI output
// side shapes + translation functions
// =============================================================================

type openAIRequest struct {
	Model       string                 `json:"model"`
	Messages    []openAIInboundMessage `json:"messages"`
	MaxTokens   *uint32                `json:"max_tokens,omitempty"`
	Temperature *float64               `json:"temperature,omitempty"`
	TopP        *float64               `json:"top_p,omitempty"`
	Stop        json.RawMessage        `json:"stop,omitempty"`
	Stream      bool                   `json:"stream,omitempty"`
}

// openAIMessage is the OpenAI-shaped message used when BUILDING a response back
// to the client (content is always a plain string).
type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// openAIInboundMessage parses an incoming OpenAI request message, whose content
// may be a JSON string OR an array of content parts.
type openAIInboundMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// contentToString normalizes an OpenAI message content field that may be a JSON
// string OR an array of content parts ([{"type":"text","text":"..."}]) — the
// latter is what OpenAI vision / multi-part requests send. Typing it as a Go
// string would fail to parse and reject the request. Non-text parts are skipped
// (this pair only carries text upstream).
func contentToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == "text" || p.Type == "" {
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	}
	return ""
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

// translateRequest converts an OpenAI body → Anthropic body. **The stream field
// is passed through as-is** to the upstream.
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
			if s := contentToString(m.Content); s != "" {
				systemParts = append(systemParts, s)
			}
		case "user", "assistant":
			out.Messages = append(out.Messages, anthropicMessage{
				Role:    m.Role,
				Content: contentToString(m.Content),
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

// translateResponse converts an Anthropic body → OpenAI body (non-streaming).
//
// Returning *domain.Usage to the caller has been moved out (goes through the
// extractor.NewAnthropic side channel); this function only translates the
// Anthropic usage into the OpenAI body's usage field (the shape OpenAI clients expect).
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

// mapStopReason converts Anthropic stop_reason → OpenAI finish_reason.
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

// anthropicIDOrGen: Anthropic's id looks like "msg_xxx"; OpenAI clients expect "chatcmpl-xxx".
func anthropicIDOrGen(anthropicID string) string {
	if anthropicID == "" {
		return "chatcmpl-" + randID()
	}
	return "chatcmpl-" + strings.TrimPrefix(anthropicID, "msg_")
}

// isAnthropicError roughly determines whether body is an error response (top-level "error" key).
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
