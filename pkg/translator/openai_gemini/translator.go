// Package openai_gemini is the Translator for OpenAI clients -> Gemini upstream.
//
// Clients send requests in OpenAI ChatCompletion format; this translator converts them
// to Gemini generateContent format for the adapter to forward upstream, then translates
// the upstream response back to OpenAI format for the client.
//
// **Supported**:
//   - chat (system/user/assistant/text content)
//   - streaming: when the client sends stream:true, the upstream call goes through
//     :streamGenerateContent?alt=sse, and responseHandler incrementally translates
//     Gemini SSE chunks into OpenAI SSE chunks (see below). Non-streaming uses
//     buffer-then-translate (translated all at once in Flush).
//
// **Not supported**: function calling / tool_use / vision (parts only support text).
//
// See translateRequest / translateResponse for the field mapping.
package openai_gemini

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/translator"
	"github.com/zereker/llm-gateway/pkg/usage/extractor"
)

type openaiGemini struct{}

func (openaiGemini) Source() domain.Protocol { return domain.ProtoOpenAI }
func (openaiGemini) Target() domain.Protocol { return domain.ProtoGemini }

func (openaiGemini) TranslateRequest(srcBody []byte) ([]byte, error) {
	return translateRequest(srcBody)
}

func (openaiGemini) NewResponseHandler() translator.ResponseHandler {
	return &responseHandler{ex: extractor.NewGemini()}
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
	roleSent bool // SSE: whether the role delta has already been sent
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
// containing incremental candidates) into an OpenAI chat.completion.chunk SSE.
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
	cand := ev.Get("candidates.0")
	if !cand.Exists() {
		// No candidate: if the prompt was blocked (blockReason non-empty), synthesize a
		// content_filter closing chunk so the client doesn't get a completely empty
		// stream (no content, no finish_reason).
		if br := ev.Get("promptFeedback.blockReason").String(); br != "" {
			var out []byte
			if !h.roleSent {
				h.roleSent = true
				out = append(out, h.chunk(map[string]any{"role": "assistant"}, "")...)
			}
			return append(out, h.chunk(map[string]any{}, "content_filter")...)
		}
		return nil
	}
	var out []byte
	if !h.roleSent {
		h.roleSent = true
		out = append(out, h.chunk(map[string]any{"role": "assistant"}, "")...)
	}
	var text strings.Builder
	cand.Get("content.parts").ForEach(func(_, p gjson.Result) bool {
		text.WriteString(p.Get("text").String())
		return true
	})
	if t := text.String(); t != "" {
		out = append(out, h.chunk(map[string]any{"content": t}, "")...)
	}
	// finishReason is only non-empty on the last chunk — only send a closing chunk with
	// finish_reason when it's non-empty.
	if raw := cand.Get("finishReason").String(); raw != "" {
		out = append(out, h.chunk(map[string]any{}, mapFinishReason(raw))...)
	}
	return out
}

// chunk builds one SSE line for an OpenAI chat.completion.chunk.
func (h *responseHandler) chunk(delta map[string]any, finish string) []byte {
	if h.id == "" {
		h.id = "chatcmpl-" + randID()
	}
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": nil}
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

// =============================================================================
// Gemini upstream-side shape
// =============================================================================

type geminiRequest struct {
	Contents          []geminiContent  `json:"contents"`
	SystemInstruction *geminiContent   `json:"systemInstruction,omitempty"`
	GenerationConfig  *geminiGenConfig `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiGenConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	MaxOutputTokens *uint32  `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
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
// OpenAI response-side shape (translated back to the client)
// =============================================================================

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

// =============================================================================
// Translation functions
// =============================================================================

// translateRequest translates an OpenAI body -> Gemini body.
//
// Field mapping:
//
//	messages[role=system]           -> systemInstruction
//	messages[role=user]             -> contents[role=user, parts[].text]
//	messages[role=assistant]        -> contents[role=model, parts[].text]
//	max_tokens                      -> generationConfig.maxOutputTokens
//	temperature                     -> generationConfig.temperature
//	top_p                           -> generationConfig.topP
//	stop (string or []string)       -> generationConfig.stopSequences []string
//
// Unsupported roles (tool / function) return an error.
func translateRequest(rawBody []byte) ([]byte, error) {
	var in openAIRequest
	if err := json.Unmarshal(rawBody, &in); err != nil {
		return nil, fmt.Errorf("openai body parse: %w", err)
	}

	out := geminiRequest{}
	for _, m := range in.Messages {
		switch m.Role {
		case "system":
			out.SystemInstruction = &geminiContent{
				Parts: []geminiPart{{Text: m.Content}},
			}
		case "assistant":
			out.Contents = append(out.Contents, geminiContent{
				Role:  "model",
				Parts: []geminiPart{{Text: m.Content}},
			})
		case "user":
			out.Contents = append(out.Contents, geminiContent{
				Role:  "user",
				Parts: []geminiPart{{Text: m.Content}},
			})
		default:
			return nil, fmt.Errorf("unsupported message role %q (v0.5 openai_gemini handles system/user/assistant only)", m.Role)
		}
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
	if hasCfg {
		out.GenerationConfig = &cfg
	}

	return json.Marshal(out)
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
func translateResponse(rawBody []byte, requestModel string) ([]byte, error) {
	var in geminiResponse
	if err := json.Unmarshal(rawBody, &in); err != nil {
		return nil, fmt.Errorf("gemini response parse: %w", err)
	}

	out := openAIResponse{
		ID:      "chatcmpl-" + randID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   requestModel,
	}
	for _, cand := range in.Candidates {
		var content string
		if len(cand.Content.Parts) > 0 {
			var b strings.Builder
			for _, p := range cand.Content.Parts {
				b.WriteString(p.Text)
			}
			content = b.String()
		}
		out.Choices = append(out.Choices, openAIChoice{
			Index:        cand.Index,
			Message:      openAIMessage{Role: "assistant", Content: content},
			FinishReason: mapFinishReason(cand.FinishReason),
		})
	}

	// No candidates (e.g. the prompt was blocked by SAFETY:
	// {"promptFeedback":{"blockReason":...}}): OpenAI clients expect choices to be a
	// **non-empty array** — marshaling a nil slice produces "choices":null, which
	// fails SDK deserialization. Synthesize a choice with empty content;
	// finish_reason=content_filter when blocked.
	if len(out.Choices) == 0 {
		finish := "stop"
		if in.PromptFeedback != nil && in.PromptFeedback.BlockReason != "" {
			finish = "content_filter"
		}
		out.Choices = []openAIChoice{{
			Index:        0,
			Message:      openAIMessage{Role: "assistant", Content: ""},
			FinishReason: finish,
		}}
	}

	if in.UsageMetadata != nil {
		out.Usage = openAIUsage{
			PromptTokens:     in.UsageMetadata.PromptTokenCount,
			CompletionTokens: in.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      in.UsageMetadata.TotalTokenCount,
		}
	}

	body, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("openai response marshal: %w", err)
	}
	return body, nil
}

func mapFinishReason(g string) string {
	switch strings.ToUpper(g) {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION":
		return "content_filter"
	case "":
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

func init() {
	translator.Register(openaiGemini{})
}
