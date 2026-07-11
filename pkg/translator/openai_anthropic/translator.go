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
//   - Only chat is supported (system/user/assistant/tool messages)
//   - Tool definitions and tool calls ARE carried across (see translateRequest)
//   - Multi-part content arrays are accepted, but only their text parts are
//     carried across; non-text image parts are dropped
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
	// Tools / tool_calls are now translated; only image multimodal content is
	// still dropped, so restrict the lossy report to that feature.
	translator.ReportLossyRequest(domain.ProtoOpenAI, domain.ProtoAnthropic, srcBody, "multimodal")
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

	// Streaming tool state: maps an Anthropic content-block index to the 0-based
	// OpenAI tool_call index. Only tool_use blocks are recorded; nextToolIdx is
	// the counter over tool_use blocks (independent of text blocks).
	toolBlocks  map[int]int
	nextToolIdx int

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
//	content_block_start → text: no output; tool_use: 1 chunk with a tool_calls
//	                      header delta (index/id/name, arguments:"")
//	content_block_delta → text_delta: delta={content:"<text>"};
//	                      input_json_delta: delta={tool_calls:[{index, function:{arguments}}]}
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
		Index        int `json:"index"`
		ContentBlock *struct {
			Type string `json:"type"`
			ID   string `json:"id,omitempty"`
			Name string `json:"name,omitempty"`
		} `json:"content_block,omitempty"`
		Delta *struct {
			Type        string `json:"type"`
			Text        string `json:"text,omitempty"`
			PartialJSON string `json:"partial_json,omitempty"`
			StopReason  string `json:"stop_reason,omitempty"`
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

	case "content_block_start":
		if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
			toolIdx := h.nextToolIdx
			h.nextToolIdx++
			if h.toolBlocks == nil {
				h.toolBlocks = make(map[int]int)
			}
			h.toolBlocks[ev.Index] = toolIdx
			delta := map[string]any{
				"tool_calls": []map[string]any{{
					"index": toolIdx,
					"id":    ev.ContentBlock.ID,
					"type":  "function",
					"function": map[string]any{
						"name":      ev.ContentBlock.Name,
						"arguments": "",
					},
				}},
			}
			h.writeChunk(out, delta, "")
		}

	case "content_block_delta":
		if ev.Delta == nil {
			return
		}
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text != "" {
				h.writeChunk(out, map[string]any{"content": ev.Delta.Text}, "")
			}
		case "input_json_delta":
			toolIdx, ok := h.toolBlocks[ev.Index]
			if !ok {
				return
			}
			delta := map[string]any{
				"tool_calls": []map[string]any{{
					"index":    toolIdx,
					"function": map[string]any{"arguments": ev.Delta.PartialJSON},
				}},
			}
			h.writeChunk(out, delta, "")
		}

	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			h.writeChunk(out, map[string]any{}, mapStopReason(ev.Delta.StopReason))
		}

	case "message_stop":
		out.WriteString("data: [DONE]\n\n")

	case "ping", "content_block_stop":
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
	Tools       []openAITool           `json:"tools,omitempty"`
	ToolChoice  json.RawMessage        `json:"tool_choice,omitempty"`
}

// openAITool is one entry of the OpenAI request "tools" array. Only function
// tools are handled; the JSON schema lives under function.parameters.
type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// openAIToolCall is one entry of an assistant message's "tool_calls" array;
// function.arguments is a JSON string (not an object).
type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// openAIMessage is the OpenAI-shaped message used when BUILDING a response back
// to the client. Content is `any` so it can be a plain string or null (OpenAI
// allows content:null when tool_calls is present).
type openAIMessage struct {
	Role      string               `json:"role"`
	Content   any                  `json:"content"`
	ToolCalls []openAIRespToolCall `json:"tool_calls,omitempty"`
}

// openAIRespToolCall is one entry of an assistant response message's
// "tool_calls" array; function.arguments is a JSON string.
type openAIRespToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// openAIInboundMessage parses an incoming OpenAI request message, whose content
// may be a JSON string OR an array of content parts.
type openAIInboundMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
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
	Tools         []anthropicTool    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
}

// anthropicTool is one entry of the Anthropic request "tools" array. The JSON
// schema lives under input_schema (the OpenAI function.parameters equivalent).
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicMessage.Content is a raw message so it can hold either the simple
// string shape (plain text messages) or a content-block array (messages that
// carry tool_use / tool_result blocks), matching whatever Anthropic expects.
type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
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
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
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

	// tools: OpenAI function tools → Anthropic tools (input_schema).
	for _, t := range in.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		out.Tools = append(out.Tools, anthropicTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: normalizeToolSchema(t.Function.Parameters),
		})
	}
	if len(in.ToolChoice) > 0 {
		out.ToolChoice = mapToolChoice(in.ToolChoice)
	}

	var systemParts []string
	for i := 0; i < len(in.Messages); i++ {
		m := in.Messages[i]
		switch m.Role {
		case "system":
			if s := contentToString(m.Content); s != "" {
				systemParts = append(systemParts, s)
			}
		case "user":
			c, err := json.Marshal(contentToString(m.Content))
			if err != nil {
				return nil, fmt.Errorf("user content marshal: %w", err)
			}
			out.Messages = append(out.Messages, anthropicMessage{Role: "user", Content: c})
		case "assistant":
			msg, err := buildAssistantMessage(m)
			if err != nil {
				return nil, err
			}
			out.Messages = append(out.Messages, msg)
		case "tool":
			// Merge consecutive tool messages into one Anthropic user turn whose
			// content array holds all their tool_result blocks.
			var blocks []any
			for i < len(in.Messages) && in.Messages[i].Role == "tool" {
				tm := in.Messages[i]
				blocks = append(blocks, map[string]any{
					"type":        "tool_result",
					"tool_use_id": tm.ToolCallID,
					"content":     contentToString(tm.Content),
				})
				i++
			}
			i-- // compensate for the outer loop's increment
			raw, err := json.Marshal(blocks)
			if err != nil {
				return nil, fmt.Errorf("tool_result marshal: %w", err)
			}
			out.Messages = append(out.Messages, anthropicMessage{Role: "user", Content: raw})
		default:
			return nil, fmt.Errorf("unsupported message role %q (handles system/user/assistant/tool only)", m.Role)
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

// normalizeToolSchema returns the OpenAI function.parameters JSON schema, or the
// Anthropic-required empty object schema {"type":"object"} when it is missing or
// empty.
func normalizeToolSchema(raw json.RawMessage) json.RawMessage {
	switch strings.TrimSpace(string(raw)) {
	case "", "null", "{}":
		return json.RawMessage(`{"type":"object"}`)
	default:
		return raw
	}
}

// mapToolChoice converts an OpenAI tool_choice → Anthropic tool_choice.
//
//	"auto"                                       → {"type":"auto"}
//	"required"                                   → {"type":"any"}
//	{"type":"function","function":{"name":"X"}}  → {"type":"tool","name":"X"}
//	"none" (and anything unrecognized)           → nil (omit; let the model decide)
func mapToolChoice(raw json.RawMessage) json.RawMessage {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto":
			return json.RawMessage(`{"type":"auto"}`)
		case "required":
			return json.RawMessage(`{"type":"any"}`)
		default: // "none" or unknown → omit
			return nil
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Function.Name != "" {
		b, err := json.Marshal(map[string]any{"type": "tool", "name": obj.Function.Name})
		if err == nil {
			return b
		}
	}
	return nil
}

// buildAssistantMessage builds an Anthropic assistant message. When the OpenAI
// message carries tool_calls, its content becomes a block array: an optional
// leading text block (only if content is non-empty) followed by one tool_use
// block per tool_call. Otherwise it keeps the simple string content shape.
func buildAssistantMessage(m openAIInboundMessage) (anthropicMessage, error) {
	if len(m.ToolCalls) == 0 {
		c, err := json.Marshal(contentToString(m.Content))
		if err != nil {
			return anthropicMessage{}, fmt.Errorf("assistant content marshal: %w", err)
		}
		return anthropicMessage{Role: "assistant", Content: c}, nil
	}

	var blocks []any
	if s := contentToString(m.Content); s != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": s})
	}
	for _, tc := range m.ToolCalls {
		// function.arguments is a JSON string; parse it into the input object.
		// Fall back to an empty object if it is absent or not a valid object.
		input := json.RawMessage(`{}`)
		if tc.Function.Arguments != "" {
			var obj map[string]any
			if json.Unmarshal([]byte(tc.Function.Arguments), &obj) == nil {
				input = json.RawMessage(tc.Function.Arguments)
			}
		}
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    tc.ID,
			"name":  tc.Function.Name,
			"input": input,
		})
	}
	raw, err := json.Marshal(blocks)
	if err != nil {
		return anthropicMessage{}, fmt.Errorf("assistant tool_use marshal: %w", err)
	}
	return anthropicMessage{Role: "assistant", Content: raw}, nil
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
	var toolCalls []openAIRespToolCall
	for _, blk := range in.Content {
		switch blk.Type {
		case "text":
			content.WriteString(blk.Text)
		case "tool_use":
			args := "{}"
			if len(blk.Input) > 0 {
				args = string(blk.Input)
			}
			tc := openAIRespToolCall{ID: blk.ID, Type: "function"}
			tc.Function.Name = blk.Name
			tc.Function.Arguments = args
			toolCalls = append(toolCalls, tc)
		}
	}

	msg := openAIMessage{Role: "assistant"}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
		// OpenAI allows content:null alongside tool_calls; use the text only if present.
		if content.Len() > 0 {
			msg.Content = content.String()
		} else {
			msg.Content = nil
		}
	} else {
		msg.Content = content.String()
	}

	out := openAIResponse{
		ID:      anthropicIDOrGen(in.ID),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []openAIChoice{
			{
				Index:        0,
				Message:      msg,
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
