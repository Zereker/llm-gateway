package extractor

import (
	"encoding/json"

	"github.com/zereker/llm-gateway/internal/domain"
)

// NewGemini constructs a Gemini-protocol usage Session.
//
// Applicable scenarios (matched by upstream protocol):
//   - openai_gemini: upstream is Gemini (OpenAI client -> Gemini upstream, currently
//     buffer-then-translate).
//
// **Gemini usage shape** (top-level usageMetadata):
//
//	{ "candidates": [...],
//	  "usageMetadata": {
//	      "promptTokenCount": 10, "candidatesTokenCount": 5, "totalTokenCount": 15
//	  } }
//
// **Only parses non-streaming JSON bodies**: this is a design choice, not a gap. For
// Gemini SSE streaming (streamGenerateContent), usage is extracted directly from the
// usageMetadata of the last frame by the openai_gemini responseHandler (see
// translateChunk in that package), bypassing this extractor entirely — so this Session
// stays JSON-only, and Final() parses the whole buf as one JSON document. Only the JSON
// path uses it.
func NewGemini() Session { return &geminiSession{} }

type geminiSession struct {
	buf   []byte
	usage *domain.Usage
}

func (s *geminiSession) Feed(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	s.buf = append(s.buf, chunk...)
}

func (s *geminiSession) Final() *domain.Usage {
	if s.usage != nil {
		return s.usage
	}
	if len(s.buf) == 0 {
		return nil
	}
	var resp struct {
		UsageMetadata json.RawMessage `json:"usageMetadata"`
	}
	if err := json.Unmarshal(s.buf, &resp); err != nil {
		return nil
	}
	if len(resp.UsageMetadata) == 0 {
		return nil
	}
	var counts struct {
		PromptTokenCount     int64 `json:"promptTokenCount"`
		CandidatesTokenCount int64 `json:"candidatesTokenCount"`
		TotalTokenCount      int64 `json:"totalTokenCount"`
	}
	if err := json.Unmarshal(resp.UsageMetadata, &counts); err != nil {
		return nil
	}
	// Raw preserves the verbatim usageMetadata object — including fields the
	// gateway doesn't model itself, e.g. thoughtsTokenCount (thinking-model
	// reasoning tokens) and cachedContentTokenCount (context-cache discount) —
	// so downstream billing can price them (docs/architecture/05-metering-billing.md §3).
	s.usage = &domain.Usage{
		Input:      counts.PromptTokenCount,
		Output:     counts.CandidatesTokenCount,
		Total:      counts.TotalTokenCount,
		Raw:        append([]byte(nil), resp.UsageMetadata...),
		Source:     domain.UsageSourceUpstream,
		Confidence: domain.UsageConfidenceExact,
	}
	return s.usage
}
