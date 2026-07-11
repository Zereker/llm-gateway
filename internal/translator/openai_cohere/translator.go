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
// below). Non-streaming uses buffer-then-translate. **Limitation**: only chat text is
// supported; tool calls/vision are not.
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
	translator.ReportLossyRequest(domain.ProtoOpenAI, domain.ProtoCohere, srcBody)
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
}

type openaiMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type cohereReq struct {
	Model            string      `json:"model"`
	Messages         []cohereMsg `json:"messages"`
	MaxTokens        *int        `json:"max_tokens,omitempty"`
	Temperature      *float64    `json:"temperature,omitempty"`
	P                *float64    `json:"p,omitempty"`
	Stream           bool        `json:"stream"`
	StopSequences    []string    `json:"stop_sequences,omitempty"`
	FrequencyPenalty *float64    `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64    `json:"presence_penalty,omitempty"`
	Seed             *int        `json:"seed,omitempty"`
	NumGenerations   *int        `json:"num_generations,omitempty"`
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
	out.Messages = make([]cohereMsg, 0, len(in.Messages))
	for _, m := range in.Messages {
		out.Messages = append(out.Messages, cohereMsg{Role: m.Role, Content: contentToString(m.Content)})
	}
	return json.Marshal(out)
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
	case "content-delta":
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
	default: // content-start / content-end etc.: skip
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

	// concatenate message.content[].text
	var text strings.Builder
	root.Get("message.content").ForEach(func(_, part gjson.Result) bool {
		if part.Get("type").String() == "text" {
			text.WriteString(part.Get("text").String())
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

	resp := map[string]any{
		"id":     id,
		"object": "chat.completion",
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
