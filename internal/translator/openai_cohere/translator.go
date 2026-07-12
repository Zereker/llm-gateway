// Package openai_cohere is the Translator from the OpenAI client protocol to the Cohere v2 upstream.
//
// The Cohere v2 /v2/chat request is close to OpenAI's (messages: role+content), but the
// **response shape differs**: message.content is an array ([{type:"text",text}]), usage is
// nested under usage.tokens.{input,output}_tokens, and finish_reason is an uppercase enum.
// This translator translates the request over and translates the response back to an
// OpenAI ChatCompletion for the client.
//
// **Streaming**: when the client sets stream:true, Cohere returns an SSE event stream with
// a type field; responseHandler incrementally translates it into OpenAI SSE chunks (see
// below). Non-streaming uses buffer-then-translate.
//
// **Tool calling**: Cohere v2's tools/tool_calls shapes are (verified against the official
// cohere-python SDK type definitions) structurally identical to OpenAI's
// ({type:"function",function:{name,description,parameters}} request-side;
// {id,type:"function",function:{name,arguments}} response-side, arguments already a JSON
// string) — so tools pass through mostly as-is. tool_choice is lossier: Cohere v2 only has
// two explicit values (REQUIRED/NONE), no "auto" and no per-tool force. Cohere's own
// tool_plan (reasoning text emitted before a tool call) and citations have no OpenAI
// equivalent and are not translated.
//
// **Vision**: Cohere v2's ImageContent/ImageUrl types (verified against the
// official cohere-python SDK) are structurally identical to OpenAI's
// image_url content part, so a user message's image_url parts pass through
// almost unchanged (see buildUserContent) — no reshaping needed.
//
// **Reasoning**: command-a-reasoning-08-2025 (verified against a real captured
// cassette, langchain-ai/langchain-cohere MIT) emits a "thinking" content block
// ahead of its final text/tool_calls block; unlike Anthropic's extended
// thinking it carries no signature. It's surfaced on the response as
// reasoning_content (matching the openai_anthropic convention), but — like
// Vercel AI SDK's own Cohere provider — is not sent back on history replay:
// Cohere's request schema has no equivalent inbound field, and dropping it
// costs nothing since there's no signature Cohere would reject a tool_use
// without.
//
// Other client protocols (Anthropic / Responses) reach this via the OpenAI pivot composition
// (see translator.compose).
package openai_cohere

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/translator"
)

type openaiCohere struct{}

// New returns the OpenAI-to-Cohere translator.
func New() translator.Translator { return openaiCohere{} }

func (openaiCohere) Source() domain.Protocol { return domain.ProtoOpenAI }
func (openaiCohere) Target() domain.Protocol { return domain.ProtoCohere }

func (openaiCohere) TranslateRequest(srcBody []byte) ([]byte, error) {
	return translateRequest(srcBody)
}

func (openaiCohere) NewResponseHandler() translator.ResponseHandler {
	return &responseHandler{}
}

// =============================================================================
// Request: OpenAI ChatCompletion -> Cohere v2 /v2/chat
// =============================================================================

type openaiReq struct {
	Model            string          `json:"model"`
	Messages         []openaiMsg     `json:"messages"`
	MaxTokens        *int            `json:"max_tokens,omitempty"`
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"top_p,omitempty"`
	Stop             json.RawMessage `json:"stop,omitempty"`
	Stream           bool            `json:"stream,omitempty"`
	FrequencyPenalty *float64        `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64        `json:"presence_penalty,omitempty"`
	Seed             *int            `json:"seed,omitempty"`
	N                *int            `json:"n,omitempty"`
	Tools            json.RawMessage `json:"tools,omitempty"`
	ToolChoice       json.RawMessage `json:"tool_choice,omitempty"`
}

type openaiMsg struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"`
	ToolCalls  []openaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

// openaiToolCall is one entry of an assistant message's "tool_calls" array in
// the request history; function.arguments is a JSON string, matching Cohere's
// own ToolCallV2Function.arguments shape (verified against cohere-python's
// type definitions) — no reserialization needed either direction.
type openaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type cohereReq struct {
	Model            string          `json:"model"`
	Messages         []cohereMsg     `json:"messages"`
	MaxTokens        *int            `json:"max_tokens,omitempty"`
	Temperature      *float64        `json:"temperature,omitempty"`
	P                *float64        `json:"p,omitempty"`
	Stream           bool            `json:"stream"`
	StopSequences    []string        `json:"stop_sequences,omitempty"`
	FrequencyPenalty *float64        `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64        `json:"presence_penalty,omitempty"`
	Seed             *int            `json:"seed,omitempty"`
	NumGenerations   *int            `json:"num_generations,omitempty"`
	Tools            json.RawMessage `json:"tools,omitempty"`
	ToolChoice       *string         `json:"tool_choice,omitempty"`
}

type cohereMsg struct {
	Role       string              `json:"role"`
	Content    any                 `json:"content,omitempty"` // string; omitted for an assistant message that only carries tool_calls
	ToolCalls  []cohereToolCallOut `json:"tool_calls,omitempty"`
	ToolCallID string              `json:"tool_call_id,omitempty"`
}

type cohereToolCallOut struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function cohereToolCallFunc `json:"function"`
}

type cohereToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func translateRequest(srcBody []byte) ([]byte, error) {
	var in openaiReq
	if err := json.Unmarshal(srcBody, &in); err != nil {
		return nil, err
	}

	out := cohereReq{
		Model:            in.Model,
		MaxTokens:        in.MaxTokens,
		Temperature:      in.Temperature,
		P:                in.TopP,
		Stream:           in.Stream, // pass through the client's stream flag
		FrequencyPenalty: in.FrequencyPenalty,
		PresencePenalty:  in.PresencePenalty,
		Seed:             in.Seed,
		NumGenerations:   in.N,
	}
	if len(in.Stop) > 0 {
		out.StopSequences = parseStopField(in.Stop)
	}

	if len(in.Tools) > 0 {
		// Cohere v2's tool definition shape is identical to OpenAI's
		// {type:"function",function:{name,description,parameters}} — verified
		// against cohere-python's ToolV2/ToolV2Function types — so no
		// transformation is needed.
		out.Tools = in.Tools
	}

	if len(in.ToolChoice) > 0 {
		out.ToolChoice = mapToolChoice(in.ToolChoice)
	}

	out.Messages = make([]cohereMsg, 0, len(in.Messages))
	for _, m := range in.Messages {
		switch m.Role {
		case "assistant":
			cm := cohereMsg{Role: "assistant"}
			if text := contentToString(m.Content); text != "" {
				cm.Content = text
			}

			for _, tc := range m.ToolCalls {
				cm.ToolCalls = append(cm.ToolCalls, cohereToolCallOut{
					ID:   tc.ID,
					Type: "function",
					Function: cohereToolCallFunc{
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				})
			}

			out.Messages = append(out.Messages, cm)
		case "tool":
			// ToolChatMessageV2 requires tool_call_id to associate the result
			// with the call it answers — the old code sent {role:"tool",
			// content} with no tool_call_id at all, so Cohere had no way to
			// match it back to a pending call.
			out.Messages = append(out.Messages, cohereMsg{
				Role:       "tool",
				ToolCallID: m.ToolCallID,
				Content:    contentToString(m.Content),
			})
		case "user":
			out.Messages = append(out.Messages, cohereMsg{Role: "user", Content: buildUserContent(m.Content)})
		default: // system
			out.Messages = append(out.Messages, cohereMsg{Role: m.Role, Content: contentToString(m.Content)})
		}
	}

	return json.Marshal(out)
}

// buildUserContent converts an OpenAI user message's content into Cohere v2
// shape: a plain string when there's no image_url part, or the content-part
// array passed through mostly as-is when one is present — Cohere v2's
// ImageContent/ImageUrl types (verified against cohere-python's SDK type
// definitions) are structurally identical to OpenAI's image_url content
// part ({"type":"image_url","image_url":{"url":...,"detail"?:...}}), so no
// reshaping is needed, only filtering to the two part types Cohere accepts.
func buildUserContent(raw json.RawMessage) any {
	r := gjson.ParseBytes(raw)
	if !r.IsArray() {
		return contentToString(raw)
	}

	hasImage := false
	r.ForEach(func(_, part gjson.Result) bool {
		if part.Get("type").String() == "image_url" {
			hasImage = true
			return false
		}

		return true
	})

	if !hasImage {
		return contentToString(raw)
	}

	var parts []json.RawMessage
	r.ForEach(func(_, part gjson.Result) bool {
		switch part.Get("type").String() {
		case "image_url", "text":
			parts = append(parts, json.RawMessage(part.Raw))
		}

		return true
	})

	return parts
}

// mapToolChoice converts an OpenAI tool_choice to Cohere v2's tool_choice.
// Cohere v2 only has two explicit values — REQUIRED and NONE — no "auto" (the
// model already decides freely without the field) and no way to force one
// specific named tool, so an OpenAI {"type":"function","function":{"name":X}}
// choice maps to the closest available: REQUIRED (forces *some* tool call,
// not necessarily X).
func mapToolChoice(raw json.RawMessage) *string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "required":
			v := "REQUIRED"
			return &v
		case "none":
			v := "NONE"
			return &v
		default: // "auto" or unrecognized -> omit, let Cohere decide
			return nil
		}
	}

	var obj struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Type == "function" {
		v := "REQUIRED"
		return &v
	}

	return nil
}

// parseStopField normalizes the OpenAI stop field (which may be a string or
// []string) into Cohere's stop_sequences []string.
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

// contentToString flattens OpenAI content (a string or a multimodal array) into plain text.
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
// Response: Cohere v2 -> OpenAI ChatCompletion (buffer-then-translate)
// =============================================================================

// responseHandler adapts to the upstream format:
//   - JSON (non-streaming): buffer-then-translate, Flush translates it into an OpenAI
//     ChatCompletion in one shot.
//   - SSE (streaming, client stream:true): incrementally translates Cohere v2 events into
//     OpenAI SSE chunks.
//
// The mode is sniffed from the first non-empty byte of the first chunk: '{' -> JSON;
// otherwise -> SSE.
type respMode int

const (
	modeUnknown respMode = iota
	modeJSON
	modeSSE
)

type responseHandler struct {
	mode    respMode
	buf     []byte // accumulates in JSON mode / staging buffer while mode is undetermined
	lineBuf []byte // line buffer for SSE mode (retains a partial line across Feed calls)
	id      string
	usage   *domain.Usage
	// contentType tracks each content block's type (as announced by its content-start
	// event) by index, since content-delta only repeats the changed field (text or
	// thinking), not the type — verified against a real command-a-reasoning-08-2025
	// cassette (langchain-ai/langchain-cohere, MIT) and cross-checked against Vercel AI
	// SDK's Cohere provider parser (packages/cohere/src/cohere-chat-language-model.ts).
	contentType map[int64]string
}

func (h *responseHandler) Feed(chunk []byte) ([]byte, error) {
	switch h.mode {
	case modeJSON:
		h.buf = append(h.buf, chunk...)
		return nil, nil
	case modeSSE:
		h.lineBuf = append(h.lineBuf, chunk...)
		return h.drainSSE(), nil
	default: // undetermined: sniff it
		h.buf = append(h.buf, chunk...)

		t := bytes.TrimLeft(h.buf, " \t\r\n")
		if len(t) == 0 {
			return nil, nil // haven't seen a non-empty byte yet
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
		// The last frame may lack a trailing newline (upstream abrupt close): drainSSE only
		// emits complete lines terminated by \n, so here we treat the leftover as the final
		// line to avoid losing the usage/finish_reason carried by message-end.
		if rest := bytes.TrimSpace(h.lineBuf); len(rest) > 0 {
			h.lineBuf = nil
			if bytes.HasPrefix(rest, []byte("data:")) {
				if data := bytes.TrimSpace(rest[len("data:"):]); len(data) > 0 {
					out = append(out, h.translateEvent(data)...)
				}
			}
		}

		out = append(out, "data: [DONE]\n\n"...) // OpenAI stream terminator

		return out, h.usage, nil
	default: // JSON / empty
		if len(h.buf) == 0 {
			return nil, nil, nil
		}
		// error path: on success, message is an object; on error (message is a string or
		// missing), pass it through as-is to preserve visibility.
		if !gjson.GetBytes(h.buf, "message").IsObject() {
			return h.buf, nil, nil
		}

		body, usage := translateResponse(h.buf)

		return body, usage, nil
	}
}

// drainSSE extracts complete lines from lineBuf and translates Cohere `data:` events into
// OpenAI SSE chunks.
func (h *responseHandler) drainSSE() []byte {
	var out []byte
	for {
		i := bytes.IndexByte(h.lineBuf, '\n')
		if i < 0 {
			break // partial line, leave it for next time
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

// translateEvent translates a single Cohere v2 stream event into an OpenAI SSE chunk.
func (h *responseHandler) translateEvent(data []byte) []byte {
	ev := gjson.ParseBytes(data)
	switch ev.Get("type").String() {
	case "message-start":
		return h.chunk(map[string]any{"role": "assistant"}, "")
	case "content-start":
		if h.contentType == nil {
			h.contentType = make(map[int64]string)
		}

		h.contentType[ev.Get("index").Int()] = ev.Get("delta.message.content.type").String()

		return nil
	case "content-delta":
		idx := ev.Get("index").Int()
		// command-a-reasoning-08-2025 emits a "thinking" content block (Cohere's
		// analog of Anthropic extended thinking) ahead of the final text/tool_calls;
		// unlike Anthropic it carries no signature to replay back in history.
		if h.contentType[idx] == "thinking" {
			thinking := ev.Get("delta.message.content.thinking").String()
			if thinking == "" {
				return nil
			}

			return h.chunk(map[string]any{"reasoning_content": thinking}, "")
		}

		text := ev.Get("delta.message.content.text").String()
		if text == "" {
			return nil
		}

		return h.chunk(map[string]any{"content": text}, "")
	case "message-end":
		usageResult := ev.Get("delta.usage")
		in := usageResult.Get("tokens.input_tokens").Int()
		outTok := usageResult.Get("tokens.output_tokens").Int()
		// message-end carries Cohere's exact, natively-reported token counts —
		// Source=upstream (not derived by us), Confidence=exact. Raw preserves
		// the verbatim usage object, including billed_units (Cohere's actually
		// -charged count, which can differ from the raw tokens count) for
		// downstream billing to price (docs/architecture/05-metering-billing.md §3).
		var rawUsage []byte
		if usageResult.Exists() {
			rawUsage = []byte(usageResult.Raw)
		}

		h.usage = &domain.Usage{Input: in, Output: outTok, Total: in + outTok, Raw: rawUsage, Source: domain.UsageSourceUpstream, Confidence: domain.UsageConfidenceExact}

		return h.chunk(map[string]any{}, mapFinishReason(ev.Get("delta.finish_reason").String()))
	case "tool-call-start":
		// delta.message.tool_calls is a single object here (not an array) —
		// verified against cohere-python's ChatToolCallStartEventDeltaMessage
		// type. index tracks which parallel call this belongs to, matching
		// OpenAI's own streaming tool_calls[].index convention.
		tc := ev.Get("delta.message.tool_calls")

		return h.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": ev.Get("index").Int(),
			"id":    tc.Get("id").String(),
			"type":  "function",
			"function": map[string]any{
				"name":      tc.Get("function.name").String(),
				"arguments": "",
			},
		}}}, "")
	case "tool-call-delta":
		frag := ev.Get("delta.message.tool_calls.function.arguments").String()
		if frag == "" {
			return nil
		}

		return h.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index":    ev.Get("index").Int(),
			"function": map[string]any{"arguments": frag},
		}}}, "")
	case "tool-call-end", "tool-plan-delta", "citation-start", "citation-end":
		// tool-call-end carries no new content (index only). tool-plan-delta
		// is Cohere's own "reasoning before calling a tool" channel and
		// citations are Cohere's grounded-RAG source attributions — neither
		// has an OpenAI-compatible equivalent, so both are intentionally
		// dropped rather than guessed at.
		return nil
	default: // content-end etc.: skip
		return nil
	}
}

// chunk builds a single OpenAI chat.completion.chunk SSE line.
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

	// concatenate message.content[].text / .thinking (command-a-reasoning-08-2025 emits a
	// "thinking" block ahead of the text/tool_calls block; see translateEvent for the
	// streaming equivalent and its real-data provenance).
	var text, reasoning strings.Builder
	root.Get("message.content").ForEach(func(_, part gjson.Result) bool {
		switch part.Get("type").String() {
		case "text":
			text.WriteString(part.Get("text").String())
		case "thinking":
			reasoning.WriteString(part.Get("thinking").String())
		}

		return true
	})

	usageResult := root.Get("usage")
	in := usageResult.Get("tokens.input_tokens").Int()
	outTok := usageResult.Get("tokens.output_tokens").Int()

	var rawUsage []byte
	if usageResult.Exists() {
		rawUsage = []byte(usageResult.Raw)
	}

	usage := &domain.Usage{
		Input:      in,
		Output:     outTok,
		Total:      in + outTok,
		Raw:        rawUsage,
		Source:     domain.UsageSourceUpstream,
		Confidence: domain.UsageConfidenceExact,
	}

	id := root.Get("id").String()
	if id == "" {
		id = "chatcmpl-" + randHex(8)
	}

	message := map[string]any{"role": "assistant"}
	if toolCalls := extractToolCalls(root.Get("message.tool_calls")); len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		// OpenAI's convention when tool_calls carries the turn: content is
		// null unless the model also emitted user-visible text alongside it.
		if text.Len() > 0 {
			message["content"] = text.String()
		} else {
			message["content"] = nil
		}
	} else {
		message["content"] = text.String()
	}

	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}

	resp := map[string]any{
		"id":     id,
		"object": "chat.completion",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
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

// extractToolCalls converts Cohere's message.tool_calls array into OpenAI's
// tool_calls shape. The two are structurally identical (verified against
// cohere-python's ToolCallV2/ToolCallV2Function types: id, type:"function",
// function.name, function.arguments as a JSON string) — this is a field
// rename, not a reshape.
func extractToolCalls(arr gjson.Result) []map[string]any {
	if !arr.IsArray() {
		return nil
	}

	var out []map[string]any
	arr.ForEach(func(_, tc gjson.Result) bool {
		out = append(out, map[string]any{
			"id":   tc.Get("id").String(),
			"type": "function",
			"function": map[string]any{
				"name":      tc.Get("function.name").String(),
				"arguments": tc.Get("function.arguments").String(),
			},
		})

		return true
	})

	return out
}

// mapFinishReason maps Cohere's uppercase enum to OpenAI's. Every documented
// Cohere v2 value (COMPLETE/STOP_SEQUENCE/MAX_TOKENS/TOOL_CALL/ERROR/TIMEOUT)
// is mapped explicitly — the previous strings.ToLower(c) fallback produced
// "tool_call" (singular) for Cohere's TOOL_CALL, which is not a valid OpenAI
// finish_reason ("tool_calls") and broke client enum matching.
func mapFinishReason(c string) string {
	switch c {
	case "COMPLETE", "STOP_SEQUENCE", "":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "TOOL_CALL":
		return "tool_calls"
	case "ERROR", "TIMEOUT":
		// OpenAI's enum has no error/timeout member; "stop" is the closest safe
		// default (callers should also check the HTTP status / error body).
		return "stop"
	default:
		return "stop"
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)

	return hex.EncodeToString(b)
}
