// Package openai_gemini is the Translator for OpenAI clients -> Gemini upstream.
//
// Clients send requests in OpenAI ChatCompletion format; this translator converts them
// to Gemini generateContent format for the adapter to forward upstream, then translates
// the upstream response back to OpenAI format for the client.
//
// **Supported**:
//
//   - chat (system/user/assistant/text content)
//
//   - streaming: when the client sends stream:true, the upstream call goes through
//     :streamGenerateContent?alt=sse, and responseHandler incrementally translates
//     Gemini SSE chunks into OpenAI SSE chunks (see below). Non-streaming uses
//     buffer-then-translate (translated all at once in Flush).
//
//   - function calling: tools -> Gemini tools[].functionDeclarations, tool_choice ->
//     toolConfig.functionCallingConfig (mode + allowedFunctionNames — verified against
//     the official generativelanguage v1beta content.proto and LiteLLM's
//     convert_to_gemini_tool_call_invoke/_result). Gemini's functionCall.args and
//     functionResponse.response are JSON **objects**, not strings — the one asymmetry
//     from OpenAI/Anthropic/Cohere's function.arguments string, handled by
//     parsing/serializing at the boundary. Content.role is only ever "user" or "model"
//     (per the proto; there is no "tool"/"function" role) — a tool result becomes a
//     "user" turn carrying a functionResponse part, matching how consecutive tool
//     messages already get merged into one turn elsewhere in this codebase
//     (openai_anthropic does the same for Anthropic).
//
//   - vision: image_url content parts -> inlineData (data: URI, base64
//     decoded) or fileData (plain URL, Gemini fetches it directly) parts.
//
// See translateRequest / translateResponse for the field mapping.
package openai_gemini

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/translator"
	"github.com/zereker/llm-gateway/internal/usage/extractor"
)

type openaiGemini struct{}

// New returns the OpenAI-to-Gemini translator.
func New() translator.Translator { return openaiGemini{} }

func (openaiGemini) Source() domain.Protocol { return domain.ProtoOpenAI }
func (openaiGemini) Target() domain.Protocol { return domain.ProtoGemini }

func (openaiGemini) TranslateRequest(srcBody []byte) ([]byte, error) {
	return translateRequest(srcBody)
}

func (openaiGemini) NewResponseHandler() translator.ResponseHandler {
	return &responseHandler{ex: extractor.NewGemini(), roleSent: map[int]bool{}}
}

// responseHandler adapts to the upstream format:
//   - JSON (non-streaming): buffer-then-translate, Flush translates it all at once into
//     OpenAI ChatCompletion.
//   - SSE (streaming, :streamGenerateContent?alt=sse): incrementally translates Gemini
//     SSE chunks into OpenAI chat.completion.chunk SSE.
//
// Detected by sniffing the first non-whitespace byte: '{' -> JSON; otherwise -> SSE.
// usage: in JSON mode it goes through the extractor.Gemini side channel; in SSE mode
// it's extracted directly from the usageMetadata of the last chunk (the extractor
// doesn't parse SSE).
type respMode int

const (
	modeUnknown respMode = iota
	modeJSON
	modeSSE
)

type responseHandler struct {
	requestModel string // filled into response.model when translating back to OpenAI
	ex           extractor.Session

	mode     respMode
	buf      []byte // accumulates in JSON mode / staging buffer while mode is undetermined
	lineBuf  []byte // line buffer for SSE mode (keeps a half-line across Feed calls)
	id       string
	roleSent map[int]bool // SSE: which candidate indices have already had their role delta sent (n>1 -> multiple candidates stream in parallel)
	usage    *domain.Usage
}

func (h *responseHandler) Feed(chunk []byte) ([]byte, error) {
	switch h.mode {
	case modeJSON:
		h.buf = append(h.buf, chunk...)
		h.ex.Feed(chunk)

		return nil, nil
	case modeSSE:
		h.lineBuf = append(h.lineBuf, chunk...)
		return h.drainSSE(), nil
	default: // undetermined: sniff the first non-whitespace byte
		h.buf = append(h.buf, chunk...)

		t := bytes.TrimLeft(h.buf, " \t\r\n")
		if len(t) == 0 {
			return nil, nil // no non-whitespace byte yet
		}
		// Both '{' (single object) and '[' (Gemini's non-alt=sse JSON array stream)
		// take the buffer path as JSON — only genuine SSE (data: lines) enters
		// modeSSE. Otherwise an array would be misdetected as SSE, drainSSE would
		// find no data: lines -> silently empty response.
		if t[0] == '{' || t[0] == '[' {
			h.mode = modeJSON
			h.ex.Feed(h.buf) // feed what's already staged to the extractor

			return nil, nil
		}

		h.mode = modeSSE
		h.lineBuf = h.buf
		h.buf = nil

		return h.drainSSE(), nil
	}
}

func (h *responseHandler) Flush() ([]byte, *domain.Usage, error) {
	if h.mode == modeSSE {
		out := h.drainSSE()
		// Fallback for a last frame with no trailing newline (upstream abrupt close):
		// treat the leftover line as the final line and process it.
		if rest := bytes.TrimSpace(h.lineBuf); len(rest) > 0 {
			h.lineBuf = nil
			if bytes.HasPrefix(rest, []byte("data:")) {
				if data := bytes.TrimSpace(rest[len("data:"):]); len(data) > 0 {
					out = append(out, h.translateChunk(data)...)
				}
			}
		}

		out = append(out, "data: [DONE]\n\n"...) // OpenAI stream terminator

		return out, h.usage, nil
	}
	// JSON / empty
	if len(h.buf) == 0 {
		// Upstream returned an empty body (possible with 4xx/5xx + empty body); nothing
		// to pass through; don't error so M7 stays silent.
		return nil, nil, nil
	}

	if isGeminiError(h.buf) {
		// **error path**: the upstream returned a 4xx/5xx with an error JSON body.
		// Don't translate (the error schema differs from the success response); pass
		// the raw body through to the client as-is to preserve error visibility.
		// The status code is already the upstream's 4xx/5xx (forwarded by M7), so the
		// client sees a non-2xx status plus the Gemini error JSON.
		// The one downside: the error format isn't OpenAI shape; an errorTranslate can
		// be added later if needed.
		return h.buf, nil, nil
	}

	body, err := translateResponse(h.buf, h.requestModel)

	return body, h.ex.Final(), err
}

// drainSSE extracts complete lines from lineBuf, translating Gemini `data:` events into
// OpenAI SSE chunks.
func (h *responseHandler) drainSSE() []byte {
	var out []byte
	for {
		i := bytes.IndexByte(h.lineBuf, '\n')
		if i < 0 {
			break // half a line, keep it for next time
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

		out = append(out, h.translateChunk(data)...)
	}

	return out
}

// translateChunk translates a single Gemini SSE chunk (a complete geminiResponse JSON
// containing incremental candidates) into one or more OpenAI chat.completion.chunk SSE
// lines — one per candidate when the client requested n>1 (generationConfig.candidateCount),
// each keeping its own OpenAI choice index so the client can tell candidates apart.
// usageMetadata only appears in the last chunk.
func (h *responseHandler) translateChunk(data []byte) []byte {
	ev := gjson.ParseBytes(data)
	if um := ev.Get("usageMetadata"); um.Exists() {
		in := um.Get("promptTokenCount").Int()
		outTok := um.Get("candidatesTokenCount").Int()

		total := um.Get("totalTokenCount").Int()
		if total == 0 {
			total = in + outTok
		}

		h.usage = &domain.Usage{Input: in, Output: outTok, Total: total, Source: domain.UsageSourceUpstream, Confidence: domain.UsageConfidenceExact}
	}

	candidates := ev.Get("candidates")
	if !candidates.IsArray() || len(candidates.Array()) == 0 {
		// No candidate: if the prompt was blocked (blockReason non-empty), synthesize a
		// content_filter closing chunk so the client doesn't get a completely empty
		// stream (no content, no finish_reason).
		if br := ev.Get("promptFeedback.blockReason").String(); br != "" {
			var out []byte

			out = append(out, h.roleChunkIfNeeded(0)...)

			return append(out, h.chunk(0, map[string]any{}, "content_filter")...)
		}

		return nil
	}

	var out []byte
	candidates.ForEach(func(_, cand gjson.Result) bool {
		idx := int(cand.Get("index").Int())
		out = append(out, h.roleChunkIfNeeded(idx)...)

		var (
			text      strings.Builder
			toolCalls []any
		)
		// Gemini emits a functionCall as one complete part (name+args
		// together), not incremental argument tokens the way OpenAI/Cohere
		// stream tool calls — so each one becomes a single, fully-formed
		// tool_calls delta chunk rather than a start/delta/end sequence.
		cand.Get("content.parts").ForEach(func(_, p gjson.Result) bool {
			if fc := p.Get("functionCall"); fc.Exists() {
				args := fc.Get("args").Raw
				if args == "" {
					args = "{}"
				}

				tc := map[string]any{
					"index": len(toolCalls),
					"id":    "call_" + randID(),
					"type":  "function",
					"function": map[string]any{
						"name":      fc.Get("name").String(),
						"arguments": args,
					},
				}
				// thoughtSignature is a sibling of functionCall on the same
				// part, not nested under it (see geminiPart's doc comment).
				if sig := p.Get("thoughtSignature").String(); sig != "" {
					tc["thought_signature"] = sig
				}

				toolCalls = append(toolCalls, tc)

				return true
			}

			text.WriteString(p.Get("text").String())

			return true
		})

		if t := text.String(); t != "" {
			out = append(out, h.chunk(idx, map[string]any{"content": t}, "")...)
		}

		if len(toolCalls) > 0 {
			out = append(out, h.chunk(idx, map[string]any{"tool_calls": toolCalls}, "")...)
		}
		// finishReason is only non-empty on this candidate's last chunk — only send a
		// closing chunk with finish_reason when it's non-empty.
		if raw := cand.Get("finishReason").String(); raw != "" {
			finish := mapFinishReason(raw)
			if len(toolCalls) > 0 {
				// Trust the message content over Gemini's raw finishReason, same
				// override as the non-streaming path.
				finish = "tool_calls"
			}

			out = append(out, h.chunk(idx, map[string]any{}, finish)...)
		}

		return true
	})

	return out
}

// roleChunkIfNeeded emits the one-time role:"assistant" delta for a given
// candidate index, the first time that index is seen.
func (h *responseHandler) roleChunkIfNeeded(idx int) []byte {
	if h.roleSent[idx] {
		return nil
	}

	h.roleSent[idx] = true

	return h.chunk(idx, map[string]any{"role": "assistant"}, "")
}

// chunk builds one SSE line for an OpenAI chat.completion.chunk.
func (h *responseHandler) chunk(idx int, delta map[string]any, finish string) []byte {
	if h.id == "" {
		h.id = "chatcmpl-" + randID()
	}

	choice := map[string]any{"index": idx, "delta": delta, "finish_reason": nil}
	if finish != "" {
		choice["finish_reason"] = finish
	}

	b, _ := json.Marshal(map[string]any{
		"id": h.id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
		"model": h.requestModel, "choices": []any{choice},
	})

	return append(append([]byte("data: "), b...), '\n', '\n')
}

// =============================================================================
// OpenAI request-side shape (minimal required fields)
// =============================================================================

type openAIRequest struct {
	Model          string          `json:"model"`
	Messages       []openAIMessage `json:"messages"`
	MaxTokens      *uint32         `json:"max_tokens,omitempty"`
	Temperature    *float64        `json:"temperature,omitempty"`
	TopP           *float64        `json:"top_p,omitempty"`
	Stop           json.RawMessage `json:"stop,omitempty"`
	Stream         bool            `json:"stream,omitempty"`
	N              *int            `json:"n,omitempty"`
	ResponseFormat json.RawMessage `json:"response_format,omitempty"`
	Tools          []openAITool    `json:"tools,omitempty"`
	ToolChoice     json.RawMessage `json:"tool_choice,omitempty"`
}

// openAIMessage.Content is raw so a tool-calling assistant message (whose
// content is often null) and a plain string both unmarshal without error —
// a fixed `string` field is what used to make any null/array content hard-
// crash json.Unmarshal instead of translating or failing cleanly.
type openAIMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// openAIToolCall.Function.Arguments is a JSON string (OpenAI's convention);
// Gemini's functionCall.args is a JSON object — the conversion happens at
// the call site (json.Unmarshal into a json.RawMessage), matching LiteLLM's
// convert_to_gemini_tool_call_invoke.
type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
	// ThoughtSignature round-trips Gemini 3's per-call thoughtSignature (see
	// geminiPart's doc comment) through the OpenAI tool_calls shape.
	ThoughtSignature string `json:"thought_signature,omitempty"`
}

// contentToString extracts plain text from an OpenAI message's content field
// (a JSON string) — null (a tool-calling assistant message with no text) and
// anything else normalize to "".
func contentToString(raw json.RawMessage) string {
	var s string

	_ = json.Unmarshal(raw, &s)

	return s
}

// buildUserParts converts an OpenAI user message's content into Gemini
// parts: a single text part for plain-string content, or a text+image part
// mix when the content is an array containing image_url entries — collapsing
// to contentToString would silently drop the image.
func buildUserParts(raw json.RawMessage) []geminiPart {
	var arr []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return []geminiPart{{Text: contentToString(raw)}}
	}

	var parts []geminiPart
	for _, p := range arr {
		switch p.Type {
		case "image_url":
			if p.ImageURL != nil {
				parts = append(parts, imagePartFromURL(p.ImageURL.URL))
			}
		default: // "text" or unset
			if p.Text != "" {
				parts = append(parts, geminiPart{Text: p.Text})
			}
		}
	}

	return parts
}

// imagePartFromURL converts an OpenAI image_url.url into a Gemini part: a
// data: URI decodes into inlineData (mimeType + base64 data split out);
// anything else passes through as fileData (Gemini fetches it directly).
func imagePartFromURL(url string) geminiPart {
	if mimeType, data, ok := parseDataURI(url); ok {
		return geminiPart{InlineData: &geminiInlineData{MimeType: mimeType, Data: data}}
	}

	return geminiPart{FileData: &geminiFileData{FileURI: url}}
}

// parseDataURI splits a "data:<mimeType>;base64,<data>" URI into its parts.
func parseDataURI(url string) (mimeType, data string, ok bool) {
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

	mimeType = rest[:semi]

	encoding := rest[semi+1 : comma]
	if encoding != "base64" {
		return "", "", false
	}

	return mimeType, rest[comma+1:], true
}

// =============================================================================
// Gemini upstream-side shape
// =============================================================================

type geminiRequest struct {
	Contents          []geminiContent   `json:"contents"`
	SystemInstruction *geminiContent    `json:"systemInstruction,omitempty"`
	GenerationConfig  *geminiGenConfig  `json:"generationConfig,omitempty"`
	Tools             []geminiTool      `json:"tools,omitempty"`
	ToolConfig        *geminiToolConfig `json:"toolConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

// geminiPart is a union (Gemini's Part is a oneof): exactly one of Text /
// FunctionCall / FunctionResponse is set on any given part.
type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
	InlineData       *geminiInlineData       `json:"inlineData,omitempty"`
	FileData         *geminiFileData         `json:"fileData,omitempty"`
	// ThoughtSignature is Gemini 3's per-functionCall equivalent of
	// Anthropic's thinking-block signature: an opaque signed blob sibling to
	// a functionCall part (verified against a real captured Gemini 3
	// response — simonw/llm-gemini's
	// test_tools_with_gemini_3_thought_signatures.yaml cassette, Apache
	// 2.0) that must be replayed verbatim when that functionCall is echoed
	// back in history, or the model loses its own signed reasoning chain
	// for that call. Round-tripped via openAIToolCall/tool_calls'
	// thought_signature field (see buildAssistantMessage / translateRequest).
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
}

// geminiInlineData carries base64-inline media (verified against the
// generativelanguage v1beta content.proto Blob message: mimeType + data).
type geminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

// geminiFileData references media by URL — Gemini fetches it directly, no
// need for the gateway to proxy the bytes.
type geminiFileData struct {
	MimeType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri"`
}

// geminiFunctionCall.Args is a JSON **object**, not a string — the one
// asymmetry from OpenAI/Anthropic/Cohere's function.arguments string
// (verified against generativelanguage v1beta content.proto).
type geminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

// geminiFunctionResponse.Response is a JSON object (proto Struct), required.
// Name must match the functionCall.name it answers.
type geminiFunctionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

// geminiTool wraps every function declaration into a single Tools[] entry —
// the conventional shape (one Tool per request carrying all declarations),
// not one Tool per function.
type geminiTool struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"functionDeclarations,omitempty"`
}

type geminiFunctionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// geminiToolConfig.FunctionCallingConfig.Mode is one of AUTO (default,
// model decides) / ANY (must call a function; combined with
// AllowedFunctionNames this forces one specific function — Gemini can do
// this natively, unlike Cohere v2 which only forces *some* call) / NONE
// (forbidden to call any function).
type geminiToolConfig struct {
	FunctionCallingConfig geminiFunctionCallingConfig `json:"functionCallingConfig"`
}

type geminiFunctionCallingConfig struct {
	Mode                 string   `json:"mode"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

type geminiGenConfig struct {
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"topP,omitempty"`
	MaxOutputTokens  *uint32         `json:"maxOutputTokens,omitempty"`
	StopSequences    []string        `json:"stopSequences,omitempty"`
	CandidateCount   *int            `json:"candidateCount,omitempty"`
	ResponseMimeType string          `json:"responseMimeType,omitempty"`
	ResponseSchema   json.RawMessage `json:"responseSchema,omitempty"`
}

type geminiResponse struct {
	Candidates     []geminiCandidate `json:"candidates"`
	UsageMetadata  *geminiUsageMeta  `json:"usageMetadata,omitempty"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback,omitempty"`
}

type geminiCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason,omitempty"`
	Index        int           `json:"index"`
}

type geminiUsageMeta struct {
	PromptTokenCount     int64 `json:"promptTokenCount"`
	CandidatesTokenCount int64 `json:"candidatesTokenCount"`
	TotalTokenCount      int64 `json:"totalTokenCount"`
}

// =============================================================================
// OpenAI response-side shape (translated back to the client via map[string]any
// — see buildAssistantMessage — so content can be null when tool_calls carries
// the turn, which a fixed-string field can't represent)
// =============================================================================

// =============================================================================
// Translation functions
// =============================================================================

// translateRequest translates an OpenAI body -> Gemini body.
//
// Field mapping:
//
//	messages[role=system]           -> systemInstruction
//	messages[role=user]             -> contents[role=user, parts[].text]
//	messages[role=assistant]        -> contents[role=model, parts[].text] (+ functionCall
//	                                    parts for tool_calls)
//	messages[role=tool]             -> contents[role=user, parts[].functionResponse]
//	                                    (consecutive tool messages merge into one turn —
//	                                    Gemini expects all parallel-call results together)
//	tools                            -> tools[0].functionDeclarations
//	tool_choice                      -> toolConfig.functionCallingConfig
//	max_tokens                      -> generationConfig.maxOutputTokens
//	temperature                     -> generationConfig.temperature
//	top_p                           -> generationConfig.topP
//	stop (string or []string)       -> generationConfig.stopSequences []string
//
// Unsupported roles return an error.
func translateRequest(rawBody []byte) ([]byte, error) {
	var in openAIRequest
	if err := json.Unmarshal(rawBody, &in); err != nil {
		return nil, fmt.Errorf("openai body parse: %w", err)
	}

	out := geminiRequest{}
	// toolCallName resolves a tool result's tool_call_id back to the function
	// name it answers — OpenAI's "tool" message only carries the ID, but
	// Gemini's functionResponse requires the name (matches LiteLLM's
	// last_message_with_tool_calls tracking).
	toolCallName := map[string]string{}
	// pendingToolParts accumulates functionResponse parts across consecutive
	// "tool" messages so they land in a single Gemini "user" turn — the same
	// discipline openai_anthropic already applies to consecutive tool
	// messages, and Gemini expects all of one turn's parallel-call results
	// together rather than as separate turns.
	var pendingToolParts []geminiPart

	flushToolParts := func() {
		if len(pendingToolParts) > 0 {
			out.Contents = append(out.Contents, geminiContent{Role: "user", Parts: pendingToolParts})
			pendingToolParts = nil
		}
	}

	for _, m := range in.Messages {
		if m.Role != "tool" {
			flushToolParts()
		}

		switch m.Role {
		case "system":
			// Merge every system message into one systemInstruction — a client
			// (or an injected middleware reminder) may send more than one, and
			// replacing wholesale each time silently dropped all but the last.
			if out.SystemInstruction == nil {
				out.SystemInstruction = &geminiContent{}
			}

			out.SystemInstruction.Parts = append(out.SystemInstruction.Parts, geminiPart{Text: contentToString(m.Content)})
		case "assistant":
			var parts []geminiPart
			if text := contentToString(m.Content); text != "" {
				parts = append(parts, geminiPart{Text: text})
			}

			for _, tc := range m.ToolCalls {
				var args json.RawMessage
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
					args = json.RawMessage(`{}`)
				}

				parts = append(parts, geminiPart{
					FunctionCall:     &geminiFunctionCall{Name: tc.Function.Name, Args: args},
					ThoughtSignature: tc.ThoughtSignature,
				})
				toolCallName[tc.ID] = tc.Function.Name
			}

			out.Contents = append(out.Contents, geminiContent{Role: "model", Parts: parts})
		case "user":
			out.Contents = append(out.Contents, geminiContent{
				Role:  "user",
				Parts: buildUserParts(m.Content),
			})
		case "tool":
			pendingToolParts = append(pendingToolParts, geminiPart{FunctionResponse: &geminiFunctionResponse{
				Name:     toolCallName[m.ToolCallID],
				Response: wrapToolResultAsResponse(contentToString(m.Content)),
			}})
		default:
			return nil, fmt.Errorf("unsupported message role %q (v0.5 openai_gemini handles system/user/assistant/tool only)", m.Role)
		}
	}

	flushToolParts()

	if len(in.Tools) > 0 {
		var decls []geminiFunctionDeclaration
		for _, t := range in.Tools {
			if t.Type != "" && t.Type != "function" {
				continue
			}

			decls = append(decls, geminiFunctionDeclaration{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			})
		}

		if len(decls) > 0 {
			out.Tools = []geminiTool{{FunctionDeclarations: decls}}
		}
	}

	if len(in.ToolChoice) > 0 {
		out.ToolConfig = mapToolChoice(in.ToolChoice)
	}

	hasCfg := false

	cfg := geminiGenConfig{}
	if in.MaxTokens != nil {
		cfg.MaxOutputTokens = in.MaxTokens
		hasCfg = true
	}

	if in.Temperature != nil {
		cfg.Temperature = in.Temperature
		hasCfg = true
	}

	if in.TopP != nil {
		cfg.TopP = in.TopP
		hasCfg = true
	}

	if len(in.Stop) > 0 {
		cfg.StopSequences = parseStopField(in.Stop)
		if len(cfg.StopSequences) > 0 {
			hasCfg = true
		}
	}

	if in.N != nil {
		cfg.CandidateCount = in.N
		hasCfg = true
	}

	if len(in.ResponseFormat) > 0 {
		if mime, schema := mapResponseFormat(in.ResponseFormat); mime != "" {
			cfg.ResponseMimeType = mime
			cfg.ResponseSchema = schema
			hasCfg = true
		}
	}

	if hasCfg {
		out.GenerationConfig = &cfg
	}

	return json.Marshal(out)
}

// mapResponseFormat converts an OpenAI response_format into Gemini's
// responseMimeType/responseSchema. json_schema's schema is passed through
// as-is: Gemini's responseSchema is a restricted OpenAPI-3.0 subset (no
// $ref, limited keyword support), not full JSON Schema, so an elaborate
// client schema may still be rejected upstream — full sanitization is out of
// scope here (matches LiteLLM's apply_response_schema_transformation, which
// also passes the schema through without rewriting it).
func mapResponseFormat(raw json.RawMessage) (mimeType string, schema json.RawMessage) {
	var rf struct {
		Type       string `json:"type"`
		JSONSchema *struct {
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(raw, &rf); err != nil {
		return "", nil
	}

	switch rf.Type {
	case "json_object":
		return "application/json", nil
	case "json_schema":
		if rf.JSONSchema != nil {
			return "application/json", rf.JSONSchema.Schema
		}

		return "application/json", nil
	default: // "text" or unrecognized: no responseMimeType override
		return "", nil
	}
}

// mapToolChoice converts an OpenAI tool_choice into Gemini's toolConfig.
// Unlike Cohere v2, Gemini can natively force one *specific* named function
// via allowedFunctionNames — no lossy fallback needed for that case.
func mapToolChoice(raw json.RawMessage) *geminiToolConfig {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "required":
			return &geminiToolConfig{FunctionCallingConfig: geminiFunctionCallingConfig{Mode: "ANY"}}
		case "none":
			return &geminiToolConfig{FunctionCallingConfig: geminiFunctionCallingConfig{Mode: "NONE"}}
		default: // "auto" or unrecognized -> omit; AUTO is Gemini's default anyway
			return nil
		}
	}

	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Type == "function" && obj.Function.Name != "" {
		return &geminiToolConfig{FunctionCallingConfig: geminiFunctionCallingConfig{
			Mode:                 "ANY",
			AllowedFunctionNames: []string{obj.Function.Name},
		}}
	}

	return nil
}

// wrapToolResultAsResponse builds a Gemini functionResponse.response object
// (a required JSON object) from an OpenAI tool message's plain-string
// content: a JSON object is preserved as-is, anything else (plain text, a
// JSON array, a bare number/string) is wrapped as {"content": text} — the
// same rule LiteLLM's convert_to_gemini_tool_call_result uses.
func wrapToolResultAsResponse(content string) json.RawMessage {
	trimmed := strings.TrimSpace(content)
	if strings.HasPrefix(trimmed, "{") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(trimmed), &obj); err == nil {
			return json.RawMessage(trimmed)
		}
	}

	b, _ := json.Marshal(map[string]string{"content": content})

	return b
}

// parseStopField normalizes the OpenAI stop field (which may be a string or []string)
// into a []string.
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

// translateResponse translates a Gemini body -> OpenAI body.
//
// Field mapping:
//
//	candidates[0].content.parts[].text  -> choices[0].message.content (concat)
//	candidates[].finishReason           -> choices[].finish_reason (lowercase)
//	usageMetadata.promptTokenCount      -> usage.prompt_tokens
//	usageMetadata.candidatesTokenCount  -> usage.completion_tokens
//	usageMetadata.totalTokenCount       -> usage.total_tokens
//
// requestModel is written into response.model (Gemini doesn't return a model name).
//
// The responsibility of returning *domain.Usage to the caller has been moved out (it
// goes through the extractor.NewGemini side channel); this function is only responsible
// for translating usageMetadata into the OpenAI body's usage field (the shape OpenAI
// clients expect).
// mergeGeminiArrayStream merges Gemini's raw (non-alt=sse) streamGenerateContent
// wire format — a top-level JSON array of incremental geminiResponse objects,
// one per streamed chunk — into a single object translateResponse can parse.
// Each array element's candidates[].content.parts are concatenated in
// recording order per candidate index; the last non-empty finishReason and
// the last usageMetadata/promptFeedback seen win, matching how those fields
// only ever appear complete on Gemini's final chunk.
//
// This gateway's own Gemini session always appends alt=sse when streaming
// (see internal/protocol/gemini/session.go's geminiStreamURL), so a real
// production response never actually arrives in this shape — this exists so
// Flush's JSON-mode path (see Feed's doc comment: both '{' and '[' route
// here) matches its own stated intent instead of panicking if that
// invariant is ever broken (a custom endpoint URL, a quirks rewrite that
// strips the query string, etc.). If rawBody isn't a top-level array, it is
// returned unchanged.
func mergeGeminiArrayStream(rawBody []byte) []byte {
	trimmed := bytes.TrimLeft(rawBody, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return rawBody
	}

	var chunks []geminiResponse
	if err := json.Unmarshal(rawBody, &chunks); err != nil {
		return rawBody // let the caller's own Unmarshal surface the real parse error
	}

	merged := geminiResponse{}
	byIndex := map[int]*geminiCandidate{}

	var order []int
	for _, chunk := range chunks {
		for _, cand := range chunk.Candidates {
			existing, ok := byIndex[cand.Index]
			if !ok {
				c := geminiCandidate{Index: cand.Index, Content: geminiContent{Role: cand.Content.Role}}
				byIndex[cand.Index] = &c
				existing = &c

				order = append(order, cand.Index)
			}

			existing.Content.Parts = append(existing.Content.Parts, cand.Content.Parts...)
			if cand.FinishReason != "" {
				existing.FinishReason = cand.FinishReason
			}
		}

		if chunk.UsageMetadata != nil {
			merged.UsageMetadata = chunk.UsageMetadata
		}

		if chunk.PromptFeedback != nil {
			merged.PromptFeedback = chunk.PromptFeedback
		}
	}

	sort.Ints(order)

	for _, idx := range order {
		merged.Candidates = append(merged.Candidates, *byIndex[idx])
	}

	out, err := json.Marshal(merged)
	if err != nil {
		return rawBody
	}

	return out
}

func translateResponse(rawBody []byte, requestModel string) ([]byte, error) {
	rawBody = mergeGeminiArrayStream(rawBody)

	var in geminiResponse
	if err := json.Unmarshal(rawBody, &in); err != nil {
		return nil, fmt.Errorf("gemini response parse: %w", err)
	}

	out := map[string]any{
		"id":      "chatcmpl-" + randID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   requestModel,
	}

	var choices []map[string]any
	for _, cand := range in.Candidates {
		message, hasToolCalls := buildAssistantMessage(cand.Content.Parts)

		finish := mapFinishReason(cand.FinishReason)
		if hasToolCalls {
			// Trust the message content over Gemini's raw finishReason (often
			// just "STOP") — the same override LiteLLM's _check_finish_reason
			// applies, so an OpenAI client's tool_calls branch actually fires.
			finish = "tool_calls"
		}

		choices = append(choices, map[string]any{
			"index":         cand.Index,
			"message":       message,
			"finish_reason": finish,
		})
	}

	// No candidates (e.g. the prompt was blocked by SAFETY:
	// {"promptFeedback":{"blockReason":...}}): OpenAI clients expect choices to be a
	// **non-empty array** — marshaling a nil slice produces "choices":null, which
	// fails SDK deserialization. Synthesize a choice with empty content;
	// finish_reason=content_filter when blocked.
	if len(choices) == 0 {
		finish := "stop"
		if in.PromptFeedback != nil && in.PromptFeedback.BlockReason != "" {
			finish = "content_filter"
		}

		choices = []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": ""},
			"finish_reason": finish,
		}}
	}

	out["choices"] = choices

	usage := map[string]any{}
	if in.UsageMetadata != nil {
		usage = map[string]any{
			"prompt_tokens":     in.UsageMetadata.PromptTokenCount,
			"completion_tokens": in.UsageMetadata.CandidatesTokenCount,
			"total_tokens":      in.UsageMetadata.TotalTokenCount,
		}
	}

	out["usage"] = usage

	body, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("openai response marshal: %w", err)
	}

	return body, nil
}

// buildAssistantMessage converts Gemini content parts into an OpenAI
// assistant message. functionCall parts become tool_calls entries
// (function.arguments serialized from Gemini's object args into the JSON
// string OpenAI expects); content is null per OpenAI's own convention when
// tool_calls carries the turn and there's no accompanying text.
func buildAssistantMessage(parts []geminiPart) (message map[string]any, hasToolCalls bool) {
	var (
		text      strings.Builder
		toolCalls []map[string]any
	)
	for _, p := range parts {
		if p.FunctionCall != nil {
			args := string(p.FunctionCall.Args)
			if args == "" {
				args = "{}"
			}

			tc := map[string]any{
				"id":   "call_" + randID(),
				"type": "function",
				"function": map[string]any{
					"name":      p.FunctionCall.Name,
					"arguments": args,
				},
			}
			if p.ThoughtSignature != "" {
				// Must be replayed verbatim if this call is echoed back in
				// history (see geminiPart.ThoughtSignature's doc comment).
				tc["thought_signature"] = p.ThoughtSignature
			}

			toolCalls = append(toolCalls, tc)

			continue
		}

		text.WriteString(p.Text)
	}

	message = map[string]any{"role": "assistant"}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		if text.Len() > 0 {
			message["content"] = text.String()
		} else {
			message["content"] = nil
		}

		return message, true
	}

	message["content"] = text.String()

	return message, false
}

// mapFinishReason converts Gemini's finishReason to an OpenAI finish_reason.
// Gemini's enum (Candidate.FinishReason) has more members than OpenAI's five;
// every documented value is mapped explicitly so a new one added upstream
// fails a completeness test instead of silently collapsing into "stop".
func mapFinishReason(g string) string {
	switch strings.ToUpper(g) {
	case "STOP", "":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "LANGUAGE", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "content_filter"
	case "MALFORMED_FUNCTION_CALL":
		// The model attempted a tool call but produced invalid arguments; route
		// through the tool_calls path so the client inspects the call instead of
		// treating it as a clean stop.
		return "tool_calls"
	case "OTHER", "FINISH_REASON_UNSPECIFIED":
		return "stop"
	default:
		return "stop"
	}
}

// isGeminiError checks whether body is a Gemini error response. The Gemini error shape
// is a top-level {"error":{"code":...,"message":...,"status":...}} — this uses a
// **structural** check of whether the top-level error is an object, rather than
// scanning bytes for an `"error"` substring. The latter would misdetect a success
// response whose content happens to contain "error" (e.g. the model's reply is
// literally "error") as an error, causing it to skip translation and drop usage
// (a billing under-count).
func isGeminiError(body []byte) bool {
	return gjson.GetBytes(body, "error").IsObject()
}

func randID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)

	return hex.EncodeToString(b)
}
