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
//   - Tool definitions, tool calls, and image_url content parts are all
//     carried across (see translateRequest / buildUserContent)
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

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/translator"
	"github.com/zereker/llm-gateway/internal/usage/extractor"
)

// Anthropic Messages API requires max_tokens; used as the default when the
// client doesn't pass one. 4096 is a conservative value (claude-3-5-sonnet
// caps at 8192; haiku caps at 8192).
const defaultAnthropicMaxTokens uint32 = 4096

// Wire vocabulary this file both reads (switch/case on an upstream field) and
// writes (as a literal in a constructed map[string]any) — named once per
// value so a typo shows up as a compile error instead of a silently wrong
// JSON key/value.
const (
	roleAssistant = "assistant"
	roleTool      = "tool"
	roleUser      = "user"

	blockTypeToolUse  = "tool_use"
	blockTypeText     = "text"
	blockTypeThinking = "thinking"
	blockTypeImage    = "image"

	sourceTypeBase64 = "base64"
	sourceTypeURL    = "url"

	toolChoiceAuto = "auto"
	toolChoiceTool = "tool"

	keyType        = "type"
	keyIndex       = "index"
	keyName        = "name"
	keyFunction    = "function"
	keyToolCalls   = "tool_calls"
	keyURLCitation = "url_citation"
	keyObjectField = "object"
	keyChatObject  = "chat.completion.chunk"

	finishStop          = "stop"
	finishLength        = "length"
	finishContentFilter = "content_filter"
)

type openaiAnthropic struct{}

// New returns the OpenAI-to-Anthropic translator.
func New() translator.Translator { return openaiAnthropic{} }

func (openaiAnthropic) Source() domain.Protocol { return domain.ProtoOpenAI }
func (openaiAnthropic) Target() domain.Protocol { return domain.ProtoAnthropic }

func (openaiAnthropic) TranslateRequest(srcBody []byte) ([]byte, error) {
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

	// Streaming citation state: contentLen tracks the running length of the
	// OpenAI-shaped "content" string built so far (across all text blocks,
	// concatenated) so a citation's start_index/end_index can be computed the
	// same way as the non-streaming path (see translateResponse). citeStarts
	// records each text block's start offset (set on content_block_start);
	// pendingCites buffers that block's web_search_result_location citations
	// (the only citation type with a URL — see anthropicCitation) until
	// content_block_stop, when the block's end offset is finally known.
	contentLen   int
	citeStarts   map[int]int
	pendingCites map[int][]anthropicCitation

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
//	content_block_start → text: no output (records the block's start offset for
//	                      citations); tool_use: 1 chunk with a tool_calls header
//	                      delta (index/id/name, arguments:"")
//	content_block_delta → text_delta: delta={content:"<text>"};
//	                      input_json_delta: delta={tool_calls:[{index, function:{arguments}}]};
//	                      citations_delta: buffered, no output yet (see content_block_stop)
//	content_block_stop  → buffered web_search_result_location citations (if any):
//	                      1 chunk with delta={annotations:[...]} (OpenAI's
//	                      url_citation shape — no official streaming precedent,
//	                      so emitted as one complete chunk per block rather than
//	                      guessed-at partial deltas); otherwise no output
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
			Type        string             `json:"type"`
			Text        string             `json:"text,omitempty"`
			PartialJSON string             `json:"partial_json,omitempty"`
			StopReason  string             `json:"stop_reason,omitempty"`
			Thinking    string             `json:"thinking,omitempty"`
			Signature   string             `json:"signature,omitempty"`
			Citation    *anthropicCitation `json:"citation,omitempty"`
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
		h.writeChunk(out, map[string]any{"role": roleAssistant}, "")

	case "content_block_start":
		if ev.ContentBlock != nil && ev.ContentBlock.Type == blockTypeToolUse {
			toolIdx := h.nextToolIdx

			h.nextToolIdx++
			if h.toolBlocks == nil {
				h.toolBlocks = make(map[int]int)
			}

			h.toolBlocks[ev.Index] = toolIdx
			delta := map[string]any{
				keyToolCalls: []map[string]any{{
					keyIndex: toolIdx,
					"id":     ev.ContentBlock.ID,
					keyType:  keyFunction,
					keyFunction: map[string]any{
						keyName:     ev.ContentBlock.Name,
						"arguments": "",
					},
				}},
			}
			h.writeChunk(out, delta, "")
		}

		if ev.ContentBlock != nil && ev.ContentBlock.Type == blockTypeText {
			// Records this block's start offset into the running OpenAI
			// "content" string, so any citations_delta buffered against it
			// can compute start_index/end_index the same way the
			// non-streaming path does (see translateResponse).
			if h.citeStarts == nil {
				h.citeStarts = make(map[int]int)
			}

			h.citeStarts[ev.Index] = h.contentLen
		}

	case "content_block_delta":
		if ev.Delta == nil {
			return
		}

		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text != "" {
				h.contentLen += len(ev.Delta.Text)
				h.writeChunk(out, map[string]any{"content": ev.Delta.Text}, "")
			}
		case "citations_delta":
			// Buffered until content_block_stop, when the block's end offset
			// into the running content string is finally known.
			if ev.Delta.Citation != nil && ev.Delta.Citation.Type == "web_search_result_location" {
				if h.pendingCites == nil {
					h.pendingCites = make(map[int][]anthropicCitation)
				}

				h.pendingCites[ev.Index] = append(h.pendingCites[ev.Index], *ev.Delta.Citation)
			}
		case "thinking_delta":
			// Streamed incrementally under reasoning_content, matching how
			// OpenAI-compatible reasoning-model vendors already stream it
			// (see testdata/fieldmatrix/upstream/chat-openai-compat-reasoning.json).
			if ev.Delta.Thinking != "" {
				h.writeChunk(out, map[string]any{"reasoning_content": ev.Delta.Thinking}, "")
			}
		case "signature_delta":
			// Arrives once, complete, right before content_block_stop — the
			// client must capture it to round-trip via buildAssistantMessage
			// on its next turn, or a subsequent tool_use turn gets a 400.
			if ev.Delta.Signature != "" {
				h.writeChunk(out, map[string]any{"reasoning_signature": ev.Delta.Signature}, "")
			}
		case "input_json_delta":
			toolIdx, ok := h.toolBlocks[ev.Index]
			if !ok {
				return
			}

			delta := map[string]any{
				keyToolCalls: []map[string]any{{
					keyIndex:    toolIdx,
					keyFunction: map[string]any{"arguments": ev.Delta.PartialJSON},
				}},
			}
			h.writeChunk(out, delta, "")
		}

	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			h.writeChunk(out, map[string]any{}, mapStopReason(ev.Delta.StopReason))
		}

	case "message_stop":
		// OpenAI streams end with a usage chunk (choices:[]) before [DONE].
		// The client's stream_options.include_usage isn't visible here (the
		// handler has no request context), so it is emitted unconditionally —
		// same as the major aggregator gateways; SDKs must already tolerate
		// the empty-choices usage chunk. The extractor has the final numbers
		// by now: anthropic delivers usage in message_delta, before this event.
		h.writeUsageChunk(out)
		out.WriteString("data: [DONE]\n\n")

	case "content_block_stop":
		if cites, ok := h.pendingCites[ev.Index]; ok {
			start := h.citeStarts[ev.Index]
			end := h.contentLen

			annotations := make([]map[string]any, 0, len(cites))
			for _, c := range cites {
				annotations = append(annotations, map[string]any{
					keyType: keyURLCitation,
					keyURLCitation: map[string]any{
						sourceTypeURL: c.URL, "title": c.Title, "start_index": start, "end_index": end,
					},
				})
			}

			h.writeChunk(out, map[string]any{"annotations": annotations}, "")
			delete(h.pendingCites, ev.Index)
		}

	case "ping":
		// No output
	}
}

// writeChunk assembles one OpenAI SSE chunk and writes it to out (including the
// "data: " prefix + "\n\n" separator).
//
// Writes null when finishReason is empty; writes the string otherwise.
func (h *responseHandler) writeChunk(out *bytes.Buffer, delta map[string]any, finishReason string) {
	choice := map[string]any{
		keyIndex:        0,
		"delta":         delta,
		"finish_reason": nil,
	}
	if finishReason != "" {
		choice["finish_reason"] = finishReason
	}

	chunk := map[string]any{
		"id":           h.chatcmplID,
		keyObjectField: keyChatObject,
		"created":      h.createdSec,
		"model":        h.upstreamModel,
		"choices":      []map[string]any{choice},
	}

	b, err := json.Marshal(chunk)
	if err != nil {
		return
	}

	out.WriteString("data: ")
	out.Write(b)
	out.WriteString("\n\n")
}

// writeUsageChunk emits the final OpenAI usage chunk (empty choices + usage);
// a no-op when the upstream never reported usage.
func (h *responseHandler) writeUsageChunk(out *bytes.Buffer) {
	u := h.ex.Final()
	if u == nil {
		return
	}

	chunk := map[string]any{
		"id":           h.chatcmplID,
		keyObjectField: keyChatObject,
		"created":      h.createdSec,
		"model":        h.upstreamModel,
		"choices":      []map[string]any{},
		"usage": map[string]any{
			"prompt_tokens":     u.Input,
			"completion_tokens": u.Output,
			"total_tokens":      u.Total,
		},
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
	// ParallelToolCalls, when explicitly false, maps to Anthropic's
	// tool_choice.disable_parallel_tool_use (see applyDisableParallelToolUse).
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
}

// openAITool is one entry of the OpenAI request "tools" array. Only function
// tools are handled; the JSON schema lives under function.parameters.
type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
		Strict      *bool           `json:"strict,omitempty"`
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
	// ReasoningContent/ReasoningSignature carry an Anthropic thinking block
	// through the OpenAI wire shape — see openAIInboundMessage's doc comment.
	ReasoningContent   string `json:"reasoning_content,omitempty"`
	ReasoningSignature string `json:"reasoning_signature,omitempty"`
	// Annotations carries web_search_result_location citations through as
	// OpenAI's own web-search-citation shape (verified against the official
	// openai-python SDK's ChatCompletionMessage/Annotation/AnnotationURLCitation
	// type definitions) — see anthropicCitation's doc comment for why only
	// this one citation type maps.
	Annotations []openAIAnnotation `json:"annotations,omitempty"`
}

type openAIAnnotation struct {
	Type        string               `json:"type"`
	URLCitation openAIURLCitationOut `json:"url_citation"`
}

type openAIURLCitationOut struct {
	URL        string `json:"url"`
	Title      string `json:"title"`
	StartIndex int    `json:"start_index"`
	EndIndex   int    `json:"end_index"`
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
	// ReasoningContent/ReasoningSignature round-trip an Anthropic extended-
	// thinking block through the OpenAI wire shape (see buildAssistantMessage
	// and translateResponse) — reasoning_content matches the field name real
	// OpenAI-compatible vendors already expose (see
	// testdata/fieldmatrix/upstream/chat-openai-compat-reasoning.json);
	// reasoning_signature is Anthropic-specific, needed because Anthropic
	// rejects a tool_use block in history without a preceding *signed*
	// thinking block once extended thinking was enabled for that turn.
	ReasoningContent   string `json:"reasoning_content,omitempty"`
	ReasoningSignature string `json:"reasoning_signature,omitempty"`
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
			if p.Type == blockTypeText || p.Type == "" {
				sb.WriteString(p.Text)
			}
		}

		return sb.String()
	}

	return ""
}

// buildUserContent converts an OpenAI user message's content into Anthropic
// shape: a plain string when there's no image_url part, or a content block
// array (text + image blocks) when one is present — contentToString would
// silently drop the image otherwise.
func buildUserContent(raw json.RawMessage) (json.RawMessage, error) {
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return json.Marshal(contentToString(raw))
	}

	hasImage := false
	for _, p := range parts {
		if p.Type == "image_url" {
			hasImage = true
			break
		}
	}

	if !hasImage {
		return json.Marshal(contentToString(raw))
	}

	var blocks []any
	for _, p := range parts {
		switch p.Type {
		case "image_url":
			if p.ImageURL != nil {
				blocks = append(blocks, imageBlockFromURL(p.ImageURL.URL))
			}
		case blockTypeText, "":
			if p.Text != "" {
				blocks = append(blocks, map[string]any{keyType: blockTypeText, blockTypeText: p.Text})
			}
		}
	}

	return json.Marshal(blocks)
}

// imageBlockFromURL converts an OpenAI image_url.url into an Anthropic image
// block: a data: URI decodes into source.type=base64 (media_type + data
// split out); anything else passes through as source.type=url (Anthropic
// fetches it directly — no need for the gateway to proxy the bytes).
func imageBlockFromURL(url string) map[string]any {
	if mediaType, data, ok := parseDataURI(url); ok {
		return map[string]any{
			keyType: blockTypeImage,
			"source": map[string]any{
				keyType:      sourceTypeBase64,
				"media_type": mediaType,
				"data":       data,
			},
		}
	}

	return map[string]any{
		keyType:  blockTypeImage,
		"source": map[string]any{keyType: sourceTypeURL, sourceTypeURL: url},
	}
}

// parseDataURI splits a "data:<mediaType>;base64,<data>" URI into its parts.
func parseDataURI(url string) (mediaType, data string, ok bool) {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		return "", "", false
	}

	rest := url[len(prefix):]
	semi := strings.IndexByte(rest, ';')

	comma := strings.IndexByte(rest, ',')
	if semi < 0 || comma < 0 || comma < semi {
		return "", "", false
	}

	mediaType = rest[:semi]

	encoding := rest[semi+1 : comma]
	if encoding != sourceTypeBase64 {
		return "", "", false
	}

	return mediaType, rest[comma+1:], true
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
	// Strict mirrors OpenAI's tool-level strict flag verbatim — Anthropic's
	// tool schema accepts the same field name (verified against a real
	// captured request/response pair, langchain-ai/langchain's official
	// langchain-anthropic package, Apache 2.0, tests/cassettes/test_strict_tool_use.yaml.gz).
	Strict *bool `json:"strict,omitempty"`
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
	Type      string              `json:"type"`
	Text      string              `json:"text,omitempty"`
	ID        string              `json:"id,omitempty"`
	Name      string              `json:"name,omitempty"`
	Input     json.RawMessage     `json:"input,omitempty"`
	Thinking  string              `json:"thinking,omitempty"`
	Signature string              `json:"signature,omitempty"`
	Citations []anthropicCitation `json:"citations,omitempty"`
}

// anthropicCitation is one entry of a text block's "citations" array. Anthropic
// has several citation types (verified against a real captured citations
// response, langchain-ai/langchain's official langchain-anthropic package,
// Apache 2.0, tests/cassettes/test_citations.yaml.gz, and the Claude docs'
// web search tool page): document-grounded ones (char_location/page_location/
// content_block_location) and search_result_location only carry a
// document_index with no URL, so they have no OpenAI-compatible
// representation and are dropped. Only web_search_result_location carries a
// URL, mapping cleanly to OpenAI's annotations[].url_citation (see
// translateResponse / translateAnthropicEvent).
type anthropicCitation struct {
	Type  string `json:"type"`
	URL   string `json:"url,omitempty"`
	Title string `json:"title,omitempty"`
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
			Strict:      t.Function.Strict,
		})
	}

	if len(in.ToolChoice) > 0 {
		out.ToolChoice = mapToolChoice(in.ToolChoice)
	}

	if in.ParallelToolCalls != nil && !*in.ParallelToolCalls {
		out.ToolChoice = applyDisableParallelToolUse(out.ToolChoice)
	}

	var systemParts []string
	for i := 0; i < len(in.Messages); i++ {
		m := in.Messages[i]
		switch m.Role {
		case "system":
			if s := contentToString(m.Content); s != "" {
				systemParts = append(systemParts, s)
			}
		case roleUser:
			c, err := buildUserContent(m.Content)
			if err != nil {
				return nil, fmt.Errorf("user content marshal: %w", err)
			}

			out.Messages = append(out.Messages, anthropicMessage{Role: roleUser, Content: c})
		case roleAssistant:
			msg, err := buildAssistantMessage(m)
			if err != nil {
				return nil, err
			}

			out.Messages = append(out.Messages, msg)
		case roleTool:
			// Merge consecutive tool messages into one Anthropic user turn whose
			// content array holds all their tool_result blocks.
			var blocks []any
			for i < len(in.Messages) && in.Messages[i].Role == roleTool {
				tm := in.Messages[i]
				blocks = append(blocks, map[string]any{
					keyType:       "tool_result",
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

			out.Messages = append(out.Messages, anthropicMessage{Role: roleUser, Content: raw})
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
//	"none"                                        → {"type":"none"}
//	{"type":"function","function":{"name":"X"}}  → {"type":"tool","name":"X"}
//	anything unrecognized                        → nil (omit; let the model decide)
func mapToolChoice(raw json.RawMessage) json.RawMessage {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case toolChoiceAuto:
			return json.RawMessage(`{"type":"auto"}`)
		case "required":
			return json.RawMessage(`{"type":"any"}`)
		case "none":
			return json.RawMessage(`{"type":"none"}`)
		default: // unknown → omit
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
		b, err := json.Marshal(map[string]any{keyType: toolChoiceTool, keyName: obj.Function.Name})
		if err == nil {
			return b
		}
	}

	return nil
}

// applyDisableParallelToolUse sets disable_parallel_tool_use:true on the
// Anthropic tool_choice object, defaulting Type to "auto" (Anthropic's
// implicit default) when the client didn't send a tool_choice at all —
// Anthropic requires the flag to live on a tool_choice object, so
// parallel_tool_calls:false with no other tool_choice still needs one
// synthesized.
func applyDisableParallelToolUse(toolChoice json.RawMessage) json.RawMessage {
	m := map[string]any{keyType: toolChoiceAuto}
	if len(toolChoice) > 0 {
		_ = json.Unmarshal(toolChoice, &m)
	}

	m["disable_parallel_tool_use"] = true

	b, err := json.Marshal(m)
	if err != nil {
		return toolChoice
	}

	return b
}

// buildAssistantMessage builds an Anthropic assistant message. When the OpenAI
// message carries tool_calls or a reasoning_content/reasoning_signature pair
// (a round-tripped extended-thinking block, see translateResponse), its
// content becomes a block array: an optional leading thinking block, then an
// optional text block (only if content is non-empty), then one tool_use
// block per tool_call. Otherwise it keeps the simple string content shape.
//
// The thinking block MUST come first when present: Anthropic rejects a
// tool_use block in history with "Expected thinking or redacted_thinking
// block, but found tool_use" once extended thinking was enabled for that
// turn — the signature is what lets Anthropic verify the thinking block
// wasn't tampered with, so it must be replayed verbatim, not regenerated.
func buildAssistantMessage(m openAIInboundMessage) (anthropicMessage, error) {
	if len(m.ToolCalls) == 0 && m.ReasoningContent == "" {
		c, err := json.Marshal(contentToString(m.Content))
		if err != nil {
			return anthropicMessage{}, fmt.Errorf("assistant content marshal: %w", err)
		}

		return anthropicMessage{Role: roleAssistant, Content: c}, nil
	}

	var blocks []any
	if m.ReasoningContent != "" {
		blocks = append(blocks, map[string]any{
			keyType:           blockTypeThinking,
			blockTypeThinking: m.ReasoningContent,
			"signature":       m.ReasoningSignature,
		})
	}

	if s := contentToString(m.Content); s != "" {
		blocks = append(blocks, map[string]any{keyType: blockTypeText, blockTypeText: s})
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
			keyType: blockTypeToolUse,
			"id":    tc.ID,
			keyName: tc.Function.Name,
			"input": input,
		})
	}

	raw, err := json.Marshal(blocks)
	if err != nil {
		return anthropicMessage{}, fmt.Errorf("assistant tool_use marshal: %w", err)
	}

	return anthropicMessage{Role: roleAssistant, Content: raw}, nil
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

	var (
		content                              strings.Builder
		toolCalls                            []openAIRespToolCall
		annotations                          []openAIAnnotation
		reasoningContent, reasoningSignature string
	)
	for _, blk := range in.Content {
		switch blk.Type {
		case blockTypeText:
			start := content.Len()
			content.WriteString(blk.Text)

			end := content.Len()
			for _, c := range blk.Citations {
				if c.Type != "web_search_result_location" {
					continue // no URL to cite — see anthropicCitation's doc comment
				}

				annotations = append(annotations, openAIAnnotation{
					Type: keyURLCitation,
					URLCitation: openAIURLCitationOut{
						URL: c.URL, Title: c.Title, StartIndex: start, EndIndex: end,
					},
				})
			}
		case blockTypeToolUse:
			args := "{}"
			if len(blk.Input) > 0 {
				args = string(blk.Input)
			}

			tc := openAIRespToolCall{ID: blk.ID, Type: keyFunction}
			tc.Function.Name = blk.Name
			tc.Function.Arguments = args
			toolCalls = append(toolCalls, tc)
		case blockTypeThinking:
			// Surfaced via reasoning_content/reasoning_signature (not
			// content) so it round-trips through buildAssistantMessage on
			// the next turn instead of being silently dropped — see that
			// function's doc comment for why the signature must survive.
			reasoningContent = blk.Thinking
			reasoningSignature = blk.Signature
		}
	}

	msg := openAIMessage{Role: roleAssistant, ReasoningContent: reasoningContent, ReasoningSignature: reasoningSignature, Annotations: annotations}
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

// mapStopReason converts Anthropic stop_reason → OpenAI finish_reason. Every
// documented Anthropic value (end_turn/max_tokens/stop_sequence/tool_use/
// refusal/pause_turn) is mapped explicitly, so a new stop_reason added
// upstream fails a completeness test instead of silently collapsing into
// "stop" and losing the refusal/pause signal.
func mapStopReason(r string) string {
	switch r {
	case "end_turn", "stop_sequence", "":
		return finishStop
	case "max_tokens":
		return finishLength
	case blockTypeToolUse:
		return keyToolCalls
	case "refusal":
		// Claude declined to generate a response; content_filter is the
		// closest OpenAI-compatible signal that this isn't a clean stop.
		return finishContentFilter
	case "pause_turn":
		// A server-tool-use turn paused mid-generation; OpenAI has no
		// equivalent, so treat it like a normal stop.
		return finishStop
	default:
		return finishStop
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
