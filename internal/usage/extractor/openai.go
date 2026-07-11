package extractor

import (
	"bytes"
	"encoding/json"

	"github.com/zereker/llm-gateway/internal/domain"
)

// NewOpenAI constructs a usage Session for the OpenAI protocol.
//
// Applicable scenarios (matched by upstream protocol):
//   - identity/openai: upstream is OpenAI / OpenAI-compat
//   - anthropic_openai: upstream is OpenAI (Anthropic client -> OpenAI upstream)
//
// **OpenAI usage shape**:
//
//	{ "usage": {
//	      "prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15,
//	      "prompt_tokens_details": { "cached_tokens": 0 }
//	  } }
//
// Streaming: the data: payload of every SSE event may contain usage; only the
// last chunk (when include_usage=true) has the complete one — it keeps getting
// overwritten.
//
// Non-streaming: the complete body is parsed in one shot.
func NewOpenAI() Session { return &openaiSession{} }

type openaiSession struct {
	streamingDecided bool
	isStreaming      bool
	sseBuffer        []byte // streaming: accumulates incomplete events across chunks
	bodyBuffer       []byte // non-streaming: accumulates the complete body
	usage            *domain.Usage
}

func (s *openaiSession) Feed(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	if !s.streamingDecided {
		s.detectStreaming(chunk)
	}
	if s.isStreaming {
		s.sseBuffer = append(s.sseBuffer, chunk...)
		s.parseSSEBuffer()
	} else {
		s.bodyBuffer = append(s.bodyBuffer, chunk...)
	}
}

func (s *openaiSession) Final() *domain.Usage {
	if !s.isStreaming && s.usage == nil && len(s.bodyBuffer) > 0 {
		s.tryExtract(s.bodyBuffer)
	}
	return s.usage
}

func (s *openaiSession) detectStreaming(chunk []byte) {
	s.streamingDecided = true
	trimmed := bytes.TrimLeft(chunk, " \t\r\n")
	s.isStreaming = bytes.HasPrefix(trimmed, []byte("data:")) || bytes.HasPrefix(trimmed, []byte(":"))
}

// parseSSEBuffer splits out complete events (blank-line separated, LF or CRLF)
// and tries to extract usage from each data: line's payload.
func (s *openaiSession) parseSSEBuffer() {
	for {
		event, rest, ok := NextSSEFrame(s.sseBuffer)
		if !ok {
			return
		}
		s.sseBuffer = rest

		for _, line := range bytes.Split(event, []byte("\n")) {
			payload := extractDataPayload(line)
			if payload == nil {
				continue
			}
			if bytes.Equal(payload, []byte("[DONE]")) {
				return
			}
			s.tryExtract(payload)
		}
	}
}

// tryExtract parses a single JSON payload (could be an SSE event / a complete chat
// body / a complete image body).
//
// Handles all three shapes:
//   - chat:  {"usage":{"prompt_tokens":N,"completion_tokens":M,"total_tokens":K, ...}}
//   - image: {"created":N, "data":[{"url":"..."}, ...]}  -> stores the whole
//     payload as Raw for downstream billing
//   - neither matches -> skip
//
// `data` stays a RawMessage: some OpenAI-compat vendors return it as an
// object rather than an array (e.g. a multimodal embeddings API) — typing it
// as a slice would fail the whole unmarshal and silently drop the usage
// sitting right next to it.
func (s *openaiSession) tryExtract(payload []byte) {
	var ev struct {
		Usage json.RawMessage `json:"usage"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return
	}

	if len(ev.Usage) > 0 && !bytes.Equal(ev.Usage, []byte("null")) {
		var u openaiUsage
		if err := json.Unmarshal(ev.Usage, &u); err != nil {
			return
		}

		// Upstream returned usage exactly: source=upstream, confidence=exact.
		// Raw keeps the upstream usage object **verbatim** — re-marshaling a
		// typed struct would drop vendor extension fields (e.g. Ark's
		// prompt_tokens_details.image_tokens split) that downstream billing
		// parses per vendor / model rules (docs/05 §3).
		s.usage = &domain.Usage{
			Input:      u.input(),
			Output:     u.output(),
			Total:      u.TotalTokens,
			Raw:        append([]byte(nil), ev.Usage...),
			Source:     domain.UsageSourceUpstream,
			Confidence: domain.UsageConfidenceExact,
		}

		return
	}

	// No usage field: could be the image API; just store the whole Raw for
	// downstream billing to parse.
	if len(ev.Data) > 0 {
		s.usage = &domain.Usage{
			Raw:        payload,
			Source:     domain.UsageSourceExtracted,
			Confidence: domain.UsageConfidenceDerived,
		}
	}
}

// openaiUsage types only the counters the gateway itself needs, covering both
// official OpenAI field families — Chat Completions reports
// prompt_tokens/completion_tokens, while Images (gpt-image-1) and Responses
// report input_tokens/output_tokens. Vendors are expected to be subsets of
// these official fields; anything else (generated_images, *_tokens_details)
// travels untouched inside Raw.
type openaiUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// input returns the normalized input count: the chat family name wins, the
// images/responses family is the fallback.
func (u openaiUsage) input() int64 {
	if u.PromptTokens > 0 {
		return u.PromptTokens
	}

	return u.InputTokens
}

func (u openaiUsage) output() int64 {
	if u.CompletionTokens > 0 {
		return u.CompletionTokens
	}

	return u.OutputTokens
}
