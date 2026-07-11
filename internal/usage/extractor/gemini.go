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
		UsageMetadata *struct {
			PromptTokenCount     int64 `json:"promptTokenCount"`
			CandidatesTokenCount int64 `json:"candidatesTokenCount"`
			TotalTokenCount      int64 `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(s.buf, &resp); err != nil {
		return nil
	}
	if resp.UsageMetadata == nil {
		return nil
	}
	raw, _ := json.Marshal(resp.UsageMetadata)
	s.usage = &domain.Usage{
		Input:      resp.UsageMetadata.PromptTokenCount,
		Output:     resp.UsageMetadata.CandidatesTokenCount,
		Total:      resp.UsageMetadata.TotalTokenCount,
		Raw:        raw,
		Source:     domain.UsageSourceUpstream,
		Confidence: domain.UsageConfidenceExact,
	}
	return s.usage
}
