// Package openai_gemini is the Translator for OpenAI clients → Gemini upstream.
//
// Clients send requests in OpenAI ChatCompletion format; this translator converts them
// to Gemini generateContent format for the adapter to forward; the upstream response is
// then translated back to OpenAI format for the client.
//
// **v0.5 limitations**:
//   - Only chat is supported (system/user/assistant/text content)
//   - Streaming is not supported (buffer-then-translate at Flush; Gemini's stream format
//     differs from OpenAI SSE, so streaming translation will be added separately)
//   - function calling / tool_use / vision are not supported
//
// See translateRequest / translateResponse for the field mapping details.
package openai_gemini

import (
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

type openaiGemini struct{}

func (openaiGemini) Source() domain.Protocol { return domain.ProtoOpenAI }
func (openaiGemini) Target() domain.Protocol { return domain.ProtoGemini }

func (openaiGemini) TranslateRequest(srcBody []byte) ([]byte, error) {
	return translateRequest(srcBody)
}

func (openaiGemini) NewResponseHandler() translator.ResponseHandler {
	return &responseHandler{ex: extractor.NewGemini()}
}

// responseHandler uses a buffer-then-translate model: Feed accumulates everything,
// Flush translates it all at once.
//
// When streaming translation is added in v0.6, change this: parse the Gemini chunk JSON
// and translate it into an OpenAI SSE chunk in real time.
//
// usage goes through the extractor.Gemini side channel (factored out in v0.5 G6);
// translateResponse no longer returns usage.
type responseHandler struct {
	buf          []byte
	requestModel string // filled into response.model when translating back to OpenAI; currently left empty, which clients accept
	ex           extractor.Session
}

func (h *responseHandler) Feed(chunk []byte) ([]byte, error) {
	h.buf = append(h.buf, chunk...)
	h.ex.Feed(chunk)
	return nil, nil // buffer mode: nothing written to the client yet
}

func (h *responseHandler) Flush() ([]byte, *domain.Usage, error) {
	if len(h.buf) == 0 {
		// Upstream returned an empty body (possible with 4xx/5xx + empty body); nothing to
		// pass through; don't error, let M7 stay silent
		return nil, nil, nil
	}
	if isGeminiError(h.buf) {
		// **error path**: upstream returned 4xx/5xx with an error JSON body.
		// Don't translate (the error schema differs from the success response); pass the
		// raw body through to the client as-is, to preserve error visibility.
		// The status code is already upstream's 4xx/5xx (forwarded by M7), so the client
		// sees a non-2xx status plus the Gemini error JSON.
		// The only downside: the error format isn't OpenAI shape; add errorTranslate later if needed.
		return h.buf, nil, nil
	}
	body, err := translateResponse(h.buf, h.requestModel)
	return body, h.ex.Final(), err
}

// =============================================================================
// OpenAI inbound shape (minimal required fields)
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
// Gemini upstream shape
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
	Candidates    []geminiCandidate `json:"candidates"`
	UsageMetadata *geminiUsageMeta  `json:"usageMetadata,omitempty"`
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
// OpenAI outbound shape (translated back to the client)
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

// translateRequest converts an OpenAI body → Gemini body.
//
// Field mapping:
//
//	messages[role=system]           → systemInstruction
//	messages[role=user]             → contents[role=user, parts[].text]
//	messages[role=assistant]        → contents[role=model, parts[].text]
//	max_tokens                      → generationConfig.maxOutputTokens
//	temperature                     → generationConfig.temperature
//	top_p                           → generationConfig.topP
//	stop (string or []string)       → generationConfig.stopSequences []string
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

// parseStopField normalizes the OpenAI stop field, which may be a string or a []string,
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

// translateResponse converts a Gemini body → OpenAI body.
//
// Field mapping:
//
//	candidates[0].content.parts[].text  → choices[0].message.content (concat)
//	candidates[].finishReason           → choices[].finish_reason (lowercase)
//	usageMetadata.promptTokenCount      → usage.prompt_tokens
//	usageMetadata.candidatesTokenCount  → usage.completion_tokens
//	usageMetadata.totalTokenCount       → usage.total_tokens
//
// requestModel is written into response.model (Gemini doesn't return a model name).
//
// The responsibility of returning *domain.Usage to the caller has moved out (via the
// extractor.NewGemini side channel); this function is only responsible for translating
// usageMetadata into the OpenAI body's usage field (the shape OpenAI clients expect).
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

func isGeminiError(body []byte) bool {
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
	translator.Register(openaiGemini{})
}
