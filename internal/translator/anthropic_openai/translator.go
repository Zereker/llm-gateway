// Package anthropic_openai is the Translator from the Anthropic client protocol to the OpenAI upstream protocol.
//
// Clients send requests in the Anthropic Messages format; this translator converts them to the
// OpenAI ChatCompletion format for the adapter to forward; upstream responses are translated back
// to the Anthropic format for the client.
//
// **Two modes are supported**:
//   - Non-streaming (stream=false): buffer-then-translate; Flush translates the whole body at once.
//   - Streaming (stream=true): OpenAI SSE chunks are translated into an Anthropic SSE event sequence in real time.
//
// **Limitations**:
//   - Only chat is supported (system + user/assistant messages).
//   - Multi-block content arrays are accepted; text, tool blocks (tool_use /
//     tool_result), and image blocks are all carried across.
//   - **In streaming mode, message_start.usage.input_tokens is always 0** — OpenAI only sends the
//     usage chunk at the end, but the Anthropic protocol requires message_start to report
//     input_tokens immediately. The workaround is to send 0 first; the real input_tokens is
//     recorded via rc.Usage (accurate for the billing layer, but the client SDK sees 0).
//     v0.6 could switch to an estimate based on prompt length.
//
// See translateRequest / translateResponse / parseOpenAIChunk for the field mapping details.
package anthropic_openai

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/translator"
	"github.com/zereker/llm-gateway/internal/usage/extractor"
)

type anthropicOpenAI struct{}

// New returns the Anthropic-to-OpenAI translator.
func New() translator.Translator { return anthropicOpenAI{} }

func (anthropicOpenAI) Source() domain.Protocol { return domain.ProtoAnthropic }
func (anthropicOpenAI) Target() domain.Protocol { return domain.ProtoOpenAI }

func (anthropicOpenAI) TranslateRequest(srcBody []byte) ([]byte, error) {
	return translateRequest(srcBody)
}

func (anthropicOpenAI) NewResponseHandler() translator.ResponseHandler {
	return &responseHandler{ex: extractor.NewOpenAI()}
}

// responseHandler supports both non-streaming (buffer) and streaming (SSE event-by-event) modes.
//
// **usage has two tracks**:
//   - rc.Usage (billing): goes through the ex extractor.Session, Flush returns ex.Final().
//   - message_delta written to the client (translation output): reuses the outputTokens field,
//     updated by the translation path itself.
//
// Why two tracks: emitClosing needs to write output_tokens out as a field of the Anthropic SSE
// event, which is part of the SSE translation; whereas the Usage the caller gets is meant for
// billing / side-channel accounting. Both originate from the same OpenAI usage chunk, but since
// the consumers differ, keeping them separate is clearer.
type responseHandler struct {
	streamingDecided bool
	isStreaming      bool

	// non-streaming state
	bodyBuffer   []byte
	requestModel string

	// streaming state
	sseBuffer           []byte // incomplete event bytes accumulated across chunks
	messageID           string // the streamed response shares one msg_xxx id
	emittedMessageStart bool
	emittedClosing      bool   // whether content_block_stop + message_delta + message_stop have been sent
	upstreamModel       string // taken from the first chunk (OpenAI chunk.model)
	pendingStopReason   string // taken from the finish_reason chunk, sent with message_delta on [DONE]
	outputTokens        int64  // used for the SSE message_delta; maintained by the translation path

	// streaming content-block state. Anthropic requires every content block to be
	// framed by content_block_start / content_block_stop before the next one opens.
	// A response may contain one text block (index 0) followed by N tool_use blocks.
	nextAnthIndex  int         // running Anthropic block index assigned as blocks open
	hasOpenBlock   bool        // whether a content block is currently open
	openBlockIndex int         // Anthropic index of the currently open block (valid when hasOpenBlock)
	textStarted    bool        // whether the text block has been opened
	textBlockIndex int         // Anthropic index of the text block (assigned on first text delta)
	toolBlocks     map[int]int // OpenAI tool_call index -> Anthropic block index

	// usage side-channel (for the caller / billing)
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
		// streaming: emit the closing events if the upstream hit EOF unexpectedly without sending [DONE]
		var out bytes.Buffer
		h.emitClosing(&out)
		usage := h.ex.Final()
		if out.Len() == 0 {
			return nil, usage, nil
		}
		return out.Bytes(), usage, nil
	}

	// non-streaming
	if len(h.bodyBuffer) == 0 {
		return nil, nil, nil
	}
	if isOpenAIError(h.bodyBuffer) {
		return h.bodyBuffer, nil, nil
	}
	body, err := translateResponse(h.bodyBuffer, h.requestModel)
	return body, h.ex.Final(), err
}

// detectStreaming determines whether the response is streaming based on the prefix of the first chunk.
func (h *responseHandler) detectStreaming(chunk []byte) {
	h.streamingDecided = true
	trimmed := bytes.TrimLeft(chunk, " \t\r\n")
	h.isStreaming = bytes.HasPrefix(trimmed, []byte("data:")) || bytes.HasPrefix(trimmed, []byte(":"))
}

// parseAndEmitStream cuts out complete OpenAI SSE chunks and translates them into an Anthropic SSE event sequence.
//
// OpenAI SSE chunk shape:
//
//	data: {"id":"chatcmpl-x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{...},"finish_reason":null}]}
//	data: {"id":"chatcmpl-x",...,"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}
//	data: [DONE]
//
// Translation strategy (see translateOpenAIChunk for details):
//   - first chunk -> send message_start + content_block_start (input_tokens=0, see package comment for the limitation)
//   - delta.role / delta.content -> content_block_delta
//   - finish_reason -> stash into pendingStopReason
//   - usage chunk -> record inputTokens / outputTokens (used for rc.Usage)
//   - [DONE] -> send content_block_stop + message_delta + message_stop
func (h *responseHandler) parseAndEmitStream() []byte {
	var out bytes.Buffer
	for {
		event, rest, ok := extractor.NextSSEFrame(h.sseBuffer)
		if !ok {
			return out.Bytes()
		}
		h.sseBuffer = rest

		// extract the data: line
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

// translateOpenAIChunk parses a single OpenAI chunk JSON and emits the corresponding Anthropic event(s).
func (h *responseHandler) translateOpenAIChunk(out *bytes.Buffer, data []byte) {
	var ev struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Index int `json:"index"`
			Delta *struct {
				Role      string `json:"role,omitempty"`
				Content   string `json:"content,omitempty"`
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id,omitempty"`
					Type     string `json:"type,omitempty"`
					Function *struct {
						Name      string `json:"name,omitempty"`
						Arguments string `json:"arguments,omitempty"`
					} `json:"function,omitempty"`
				} `json:"tool_calls,omitempty"`
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

	// send message_start on the first chunk
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
					"input_tokens":  0, // see package comment: OpenAI only sends usage at the end
					"output_tokens": 1,
				},
			},
		})
	}

	for _, ch := range ev.Choices {
		// finish_reason is handled separately (it may coexist with delta, but usually only one appears per chunk)
		if ch.FinishReason != "" {
			h.pendingStopReason = mapFinishReason(ch.FinishReason)
		}
		if ch.Delta == nil {
			continue
		}
		if ch.Delta.Content != "" {
			if !h.textStarted {
				h.textStarted = true
				h.textBlockIndex = h.nextAnthIndex
				h.nextAnthIndex++
				writeEvent(out, "content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": h.textBlockIndex,
					"content_block": map[string]any{
						"type": "text",
						"text": "",
					},
				})
				h.hasOpenBlock = true
				h.openBlockIndex = h.textBlockIndex
			}
			writeEvent(out, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": h.textBlockIndex,
				"delta": map[string]any{
					"type": "text_delta",
					"text": ch.Delta.Content,
				},
			})
		}
		for _, tc := range ch.Delta.ToolCalls {
			var name, args string
			if tc.Function != nil {
				name = tc.Function.Name
				args = tc.Function.Arguments
			}
			h.emitToolCallDelta(out, tc.Index, tc.ID, name, args)
		}
		// delta.role appears in OpenAI's first chunk; Anthropic doesn't need a separate event for it (already covered by message_start)
	}

	if ev.Usage != nil {
		// the translation path only cares about outputTokens (to be written into the message_delta event);
		// inputTokens goes through the extractor side-channel to the caller, not needed here.
		h.outputTokens = ev.Usage.CompletionTokens
	}
}

// emitToolCallDelta translates a single OpenAI tool_call streaming delta into the
// Anthropic content-block events. A delta with a not-yet-seen OpenAI index carries
// the tool id + name and opens a new tool_use block (after stopping any block that
// was open); subsequent deltas for the same index carry argument fragments that
// become input_json_delta events.
func (h *responseHandler) emitToolCallDelta(out *bytes.Buffer, oaIndex int, id, name, args string) {
	if h.toolBlocks == nil {
		h.toolBlocks = make(map[int]int)
	}
	anthIdx, seen := h.toolBlocks[oaIndex]
	if !seen {
		// A new tool block starts: close the currently open block first.
		if h.hasOpenBlock {
			writeEvent(out, "content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": h.openBlockIndex,
			})
			h.hasOpenBlock = false
		}
		anthIdx = h.nextAnthIndex
		h.nextAnthIndex++
		h.toolBlocks[oaIndex] = anthIdx
		writeEvent(out, "content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": anthIdx,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    id,
				"name":  name,
				"input": map[string]any{},
			},
		})
		h.hasOpenBlock = true
		h.openBlockIndex = anthIdx
	}
	if args != "" {
		writeEvent(out, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": anthIdx,
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": args,
			},
		})
	}
}

// emitClosing sends the streaming closing trio: content_block_stop + message_delta + message_stop.
//
// Idempotent: the emittedClosing flag guarantees this is sent only once no matter how many times
// it's called (safe whether triggered early by [DONE] or as a Flush fallback).
func (h *responseHandler) emitClosing(out *bytes.Buffer) {
	if h.emittedClosing {
		return
	}
	if !h.emittedMessageStart {
		// upstream sent nothing at all -- message_start was never emitted, so skip the closing sequence
		return
	}
	h.emittedClosing = true

	// Close whatever block is still open (the text block, or the last tool_use
	// block). Earlier blocks were already stopped when the next block opened.
	if h.hasOpenBlock {
		writeEvent(out, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": h.openBlockIndex,
		})
		h.hasOpenBlock = false
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

// writeEvent writes a single SSE event: `event: <type>\ndata: <json>\n\n`.
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
// Non-streaming: Anthropic input / OpenAI upstream / Anthropic output shapes + translation functions
// =============================================================================

type anthropicRequest struct {
	Model         string             `json:"model"`
	Messages      []anthropicMessage `json:"messages"`
	System        json.RawMessage    `json:"system,omitempty"` // string or []content-block
	MaxTokens     uint32             `json:"max_tokens"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"` // {type:auto|any|tool,name?}
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []content-block
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// anthropicBlock is a single content block inside a message's content array.
// It is deliberately a union of the fields used by every block type we handle:
// text, tool_use (assistant), and tool_result (user).
type anthropicBlock struct {
	Type      string                `json:"type"`
	Text      string                `json:"text,omitempty"`
	ID        string                `json:"id,omitempty"`          // tool_use
	Name      string                `json:"name,omitempty"`        // tool_use
	Input     json.RawMessage       `json:"input,omitempty"`       // tool_use
	ToolUseID string                `json:"tool_use_id,omitempty"` // tool_result
	Content   json.RawMessage       `json:"content,omitempty"`     // tool_result (string or []block)
	Source    *anthropicImageSource `json:"source,omitempty"`      // image
}

// anthropicImageSource is an Anthropic image block's source: either
// base64-inline (media_type + data) or a direct URL (Anthropic fetches it
// itself — no need for the gateway to proxy the bytes).
type anthropicImageSource struct {
	Type      string `json:"type"` // "base64" or "url"
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// contentToString normalizes a message content / system field that may be a
// JSON string OR an array of content blocks ([{"type":"text","text":"..."}]).
// Anthropic SDKs and OpenAI vision clients routinely send the array form;
// typing the field as a Go string would fail to parse and reject the request.
// Non-text blocks (images/tools) are skipped — this pair only carries text.
func contentToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type == "text" || b.Type == "" {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	return ""
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
	Tools         []openAITool    `json:"tools,omitempty"`
	ToolChoice    any             `json:"tool_choice,omitempty"` // "auto"|"required"|"none"|{type:function,...}
	// ParallelToolCalls mirrors Anthropic's tool_choice.disable_parallel_tool_use
	// (inverted: disable_parallel_tool_use:true -> parallel_tool_calls:false).
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
}

// openAIMessage is shared by request building and response parsing. Content is a
// pointer so a tool-calling assistant message can serialize `content:null`, while
// a normal message still serializes its string.
// Content is `any` (not *string) so a user message with an image block can
// marshal as an OpenAI content-part array instead of being forced into a
// plain string, which would have to drop the image entirely.
type openAIMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAITool struct {
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

type openAIToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIToolCallFunc `json:"function"`
}

type openAIToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded arguments object as a string
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
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`    // tool_use
	Name  string          `json:"name,omitempty"`  // tool_use
	Input json.RawMessage `json:"input,omitempty"` // tool_use
}

type anthropicUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// translateRequest converts an Anthropic body to an OpenAI body.
//
// Field mapping:
//
//	system               -> inserted as messages[0] = {role:"system", content:system}
//	messages[role/content] -> messages[role/content] (role is compatible)
//	max_tokens           -> max_tokens
//	temperature/top_p    -> same name
//	stop_sequences       -> stop (OpenAI accepts []string)
//	stream               -> stream (and if true, injects stream_options.include_usage=true)
func translateRequest(rawBody []byte) ([]byte, error) {
	var in anthropicRequest
	if err := json.Unmarshal(rawBody, &in); err != nil {
		return nil, fmt.Errorf("anthropic body parse: %w", err)
	}

	out := openAIRequest{
		Model:  in.Model,
		Stream: in.Stream,
	}
	if sys := contentToString(in.System); sys != "" {
		out.Messages = append(out.Messages, openAIMessage{
			Role:    "system",
			Content: sys,
		})
	}
	for _, m := range in.Messages {
		switch m.Role {
		case "assistant":
			out.Messages = append(out.Messages, translateAssistantMessage(m.Content))
		case "user":
			out.Messages = append(out.Messages, translateUserMessage(m.Content)...)
		default:
			return nil, fmt.Errorf("unsupported message role %q (handles user/assistant only; system goes through the top-level system field)", m.Role)
		}
	}
	if len(in.Tools) > 0 {
		out.Tools = translateTools(in.Tools)
	}
	if tc := translateToolChoice(in.ToolChoice); tc != nil {
		out.ToolChoice = tc
	}
	if anthropicDisablesParallelToolUse(in.ToolChoice) {
		f := false
		out.ParallelToolCalls = &f
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

// translateTools maps Anthropic tool definitions to OpenAI function tools.
// An empty input_schema becomes the minimal {"type":"object"} JSON schema.
func translateTools(tools []anthropicTool) []openAITool {
	out := make([]openAITool, 0, len(tools))
	for _, t := range tools {
		params := t.InputSchema
		if len(bytes.TrimSpace(params)) == 0 {
			params = json.RawMessage(`{"type":"object"}`)
		}
		out = append(out, openAITool{
			Type: "function",
			Function: openAIToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}
	return out
}

// translateToolChoice maps the Anthropic tool_choice object to the OpenAI form:
//
//	{type:auto}        -> "auto"
//	{type:any}         -> "required"
//	{type:none}        -> "none"
//	{type:tool,name:X} -> {type:"function",function:{name:"X"}}
//
// Returns nil when tool_choice is absent or unrecognized (OpenAI then defaults).
func translateToolChoice(raw json.RawMessage) any {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil
	}
	switch tc.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": tc.Name},
		}
	default:
		return nil
	}
}

// anthropicDisablesParallelToolUse reports whether the client's tool_choice
// carries disable_parallel_tool_use:true, independent of the tool_choice
// type — the flag can co-occur with auto/any/tool.
func anthropicDisablesParallelToolUse(raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	var tc struct {
		DisableParallelToolUse bool `json:"disable_parallel_tool_use"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return false
	}
	return tc.DisableParallelToolUse
}

// translateAssistantMessage converts an Anthropic assistant message into a single
// OpenAI assistant message. Text blocks are concatenated into content; tool_use
// blocks become tool_calls carrying the input serialized as a JSON string.
func translateAssistantMessage(raw json.RawMessage) openAIMessage {
	blocks, ok := parseBlocks(raw)
	if !ok {
		return openAIMessage{Role: "assistant", Content: contentToString(raw)}
	}
	var text strings.Builder
	var toolCalls []openAIToolCall
	for _, b := range blocks {
		switch b.Type {
		case "text", "":
			text.WriteString(b.Text)
		case "tool_use":
			toolCalls = append(toolCalls, openAIToolCall{
				ID:   b.ID,
				Type: "function",
				Function: openAIToolCallFunc{
					Name:      b.Name,
					Arguments: rawToJSONString(b.Input),
				},
			})
		}
	}
	msg := openAIMessage{Role: "assistant", Content: text.String()}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}
	return msg
}

// translateUserMessage converts an Anthropic user message into one or more OpenAI
// messages. tool_result blocks each become a {role:"tool"} message (emitted
// first); any accompanying text blocks are emitted as a trailing {role:"user"}
// message. A plain-string or text-only message stays a single user message.
// image blocks (with no tool_result) become an OpenAI content-part array
// (text + image_url) instead of collapsing to a string, which would drop
// the image entirely.
func translateUserMessage(raw json.RawMessage) []openAIMessage {
	blocks, ok := parseBlocks(raw)
	if !ok {
		return []openAIMessage{{Role: "user", Content: contentToString(raw)}}
	}
	hasToolResult, hasImage := false, false
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			hasToolResult = true
		case "image":
			hasImage = true
		}
	}
	if hasImage && !hasToolResult {
		var parts []any
		for _, b := range blocks {
			switch b.Type {
			case "image":
				if b.Source != nil {
					parts = append(parts, imageURLPartFromSource(*b.Source))
				}
			case "text", "":
				if b.Text != "" {
					parts = append(parts, map[string]any{"type": "text", "text": b.Text})
				}
			}
		}
		return []openAIMessage{{Role: "user", Content: parts}}
	}
	if !hasToolResult {
		return []openAIMessage{{Role: "user", Content: contentToString(raw)}}
	}

	var msgs []openAIMessage
	var text strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			msgs = append(msgs, openAIMessage{
				Role:       "tool",
				ToolCallID: b.ToolUseID,
				Content:    contentToString(b.Content),
			})
		case "text", "":
			text.WriteString(b.Text)
		}
	}
	if text.Len() > 0 {
		msgs = append(msgs, openAIMessage{Role: "user", Content: text.String()})
	}
	return msgs
}

// imageURLPartFromSource converts an Anthropic image block's source into an
// OpenAI image_url content part: base64 becomes a data: URI (OpenAI has no
// separate media_type/data fields), url passes straight through.
func imageURLPartFromSource(src anthropicImageSource) map[string]any {
	url := src.URL
	if src.Type == "base64" {
		url = "data:" + src.MediaType + ";base64," + src.Data
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
}

// parseBlocks parses a message content field as an array of content blocks.
// The second return value is false when the content is a plain JSON string (or
// otherwise not a block array), in which case the caller falls back to
// contentToString — this keeps existing text-only behavior byte-for-byte.
func parseBlocks(raw json.RawMessage) ([]anthropicBlock, bool) {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, false
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, false
	}
	return blocks, true
}

// rawToJSONString normalizes a tool input object into a compact JSON string
// suitable for OpenAI's function.arguments field. An empty input becomes "{}".
func rawToJSONString(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "{}"
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "{}"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// translateResponse converts an OpenAI body to an Anthropic body (non-streaming).
//
// The responsibility of returning *domain.Usage to the caller has been moved out (handled via the
// extractor.NewOpenAI side-channel); this function only translates the OpenAI usage into the
// Anthropic body's usage field (the shape the Anthropic client expects).
func translateResponse(rawBody []byte, fallbackModel string) ([]byte, error) {
	var in openAIResponse
	if err := json.Unmarshal(rawBody, &in); err != nil {
		return nil, fmt.Errorf("openai response parse: %w", err)
	}

	model := in.Model
	if model == "" {
		model = fallbackModel
	}

	var blocks []anthropicContentBlock
	var stopReason string
	if len(in.Choices) > 0 {
		msg := in.Choices[0].Message
		// OpenAI's own chat-completion response never returns array content —
		// only a string or null — so a plain type assertion is safe here
		// (unlike the request-building side, which must also accept arrays).
		if s, ok := msg.Content.(string); ok && s != "" {
			blocks = append(blocks, anthropicContentBlock{Type: "text", Text: s})
		}
		for _, tc := range msg.ToolCalls {
			input := json.RawMessage("{}")
			if tc.Function.Arguments != "" && json.Valid([]byte(tc.Function.Arguments)) {
				input = json.RawMessage(tc.Function.Arguments)
			}
			blocks = append(blocks, anthropicContentBlock{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: input,
			})
		}
		stopReason = mapFinishReason(in.Choices[0].FinishReason)
	} else {
		stopReason = "end_turn"
	}
	if len(blocks) == 0 {
		// No content and no tool calls: keep a single empty text block (existing behavior).
		blocks = append(blocks, anthropicContentBlock{Type: "text", Text: ""})
	}

	out := anthropicResponse{
		ID:         openAIIDOrGen(in.ID),
		Type:       "message",
		Role:       "assistant",
		Model:      model,
		Content:    blocks,
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

// mapFinishReason converts an OpenAI finish_reason to an Anthropic stop_reason.
// Every documented OpenAI value (stop/length/tool_calls/content_filter, plus
// the deprecated function_call) is mapped explicitly.
func mapFinishReason(r string) string {
	switch r {
	case "stop", "":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "stop_sequence"
	case "tool_calls", "function_call":
		// function_call is the deprecated single-function precursor to
		// tool_calls; both signal the same "model wants to call a function"
		// condition, so route both to tool_use.
		return "tool_use"
	default:
		return "end_turn"
	}
}

// openAIIDOrGen converts an OpenAI id of the form "chatcmpl-xxx" to the "msg_xxx" form the Anthropic client expects.
func openAIIDOrGen(openaiID string) string {
	if openaiID == "" {
		return "msg_" + randID()
	}
	return "msg_" + strings.TrimPrefix(openaiID, "chatcmpl-")
}

// isOpenAIError roughly determines whether the body is an error response (a top-level "error" key).
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
