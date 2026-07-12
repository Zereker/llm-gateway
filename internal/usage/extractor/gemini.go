package extractor

import (
	"bytes"
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
// stays JSON-only, and Final() parses the whole buf as one JSON document (see
// extractUsageMetadata for the one exception: a top-level JSON array, Gemini's raw
// non-alt=sse streaming wire format). Only the JSON path uses it.
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
	usageMetadata := extractUsageMetadata(s.buf)
	if len(usageMetadata) == 0 {
		return nil
	}
	var counts struct {
		PromptTokenCount     int64 `json:"promptTokenCount"`
		CandidatesTokenCount int64 `json:"candidatesTokenCount"`
		TotalTokenCount      int64 `json:"totalTokenCount"`
	}
	if err := json.Unmarshal(usageMetadata, &counts); err != nil {
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
		Raw:        append([]byte(nil), usageMetadata...),
		Source:     domain.UsageSourceUpstream,
		Confidence: domain.UsageConfidenceExact,
	}
	return s.usage
}

// extractUsageMetadata reads the top-level usageMetadata field out of buf,
// which is either a single Gemini response object or — the raw
// (non-alt=sse) streamGenerateContent wire format — a JSON array of
// incremental response objects, in which case the last non-empty
// usageMetadata wins (Gemini only ever populates it fully on the final
// chunk). See mergeGeminiArrayStream in openai_gemini for the sibling case
// on the translation side; this gateway's own Gemini session always
// requests alt=sse when streaming, so a real production response never
// actually arrives in the array shape (see that function's doc comment for
// why this is still worth handling defensively).
func extractUsageMetadata(buf []byte) json.RawMessage {
	trimmed := bytes.TrimLeft(buf, " \t\r\n")
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] != '[' {
		var resp struct {
			UsageMetadata json.RawMessage `json:"usageMetadata"`
		}
		if err := json.Unmarshal(buf, &resp); err != nil {
			return nil
		}
		return resp.UsageMetadata
	}
	var chunks []struct {
		UsageMetadata json.RawMessage `json:"usageMetadata"`
	}
	if err := json.Unmarshal(buf, &chunks); err != nil {
		return nil
	}
	for i := len(chunks) - 1; i >= 0; i-- {
		if len(chunks[i].UsageMetadata) > 0 {
			return chunks[i].UsageMetadata
		}
	}
	return nil
}
