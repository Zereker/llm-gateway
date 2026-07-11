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
//   - Multi-block content arrays are accepted, but only their text blocks are
//     carried across; non-text blocks (images / tool_use) are dropped.
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

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/translator"
	"github.com/zereker/llm-gateway/pkg/usage/extractor"
)

type anthropicOpenAI struct{}

// New returns the Anthropic-to-OpenAI translator.
func New() translator.Translator { return anthropicOpenAI{} }

func (anthropicOpenAI) Source() domain.Protocol { return domain.ProtoAnthropic }
func (anthropicOpenAI) Target() domain.Protocol { return domain.ProtoOpenAI }

func (anthropicOpenAI) TranslateRequest(srcBody []byte) ([]byte, error) {
	translator.ReportLossyRequest(domain.ProtoAnthropic, domain.ProtoOpenAI, srcBody)
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
	sseBuffer                []byte // incomplete event bytes accumulated across chunks
	messageID                string // the streamed response shares one msg_xxx id
	emittedMessageStart      bool
	emittedContentBlockStart bool
	emittedClosing           bool   // whether content_block_stop + message_delta + message_stop have been sent
	upstreamModel            string // taken from the first chunk (OpenAI chunk.model)
	pendingStopReason        string // taken from the finish_reason chunk, sent with message_delta on [DONE]
	outputTokens             int64  // used for the SSE message_delta; maintained by the translation path

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
		// delta.role appears in OpenAI's first chunk; Anthropic doesn't need a separate event for it (already covered by message_start)
	}

	if ev.Usage != nil {
		// the translation path only cares about outputTokens (to be written into the message_delta event);
		// inputTokens goes through the extractor side-channel to the caller, not needed here.
		h.outputTokens = ev.Usage.CompletionTokens
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
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []content-block
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
		case "user", "assistant":
			out.Messages = append(out.Messages, openAIMessage{
				Role:    m.Role,
				Content: contentToString(m.Content),
			})
		default:
			return nil, fmt.Errorf("unsupported message role %q (handles user/assistant only; system goes through the top-level system field)", m.Role)
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

// mapFinishReason converts an OpenAI finish_reason to an Anthropic stop_reason.
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
