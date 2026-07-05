package extractor

import (
	"encoding/json"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// NewGemini constructs a usage Session for the Gemini protocol.
//
// Applicable scenarios (matched by upstream protocol):
//   - openai_gemini: upstream is Gemini (OpenAI client -> Gemini upstream,
//     currently buffer-then-translate)
//
// **Gemini usage shape** (top-level usageMetadata):
//
//	{ "candidates": [...],
//	  "usageMetadata": {
//	      "promptTokenCount": 10, "candidatesTokenCount": 5, "totalTokenCount": 15
//	  } }
//
// **v0.5 limitation**: only non-streaming body mode is supported. Gemini SSE
// streaming (streamGenerateContent) is not supported in v0.5, so SSE parsing
// isn't implemented here either. The SSE path will be added when streaming
// translation lands in v0.6.
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
