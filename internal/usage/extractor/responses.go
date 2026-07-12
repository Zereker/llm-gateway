package extractor

import (
	"bytes"
	"encoding/json"

	"github.com/zereker/llm-gateway/internal/domain"
)

// NewResponses constructs a usage Session for the OpenAI Responses protocol.
//
// Applicable scenarios (matched by upstream protocol):
//   - identity/responses: upstream speaks the Responses protocol natively
//
// **Responses usage shape** (note: field names differ from Chat Completions —
// input_tokens/output_tokens, not prompt_tokens/completion_tokens):
//
//	{ "usage": {
//	      "input_tokens": 10, "output_tokens": 5, "total_tokens": 15,
//	      "input_tokens_details": { "cached_tokens": 0 },
//	      "output_tokens_details": { "reasoning_tokens": 3 }
//	  } }
//
// Streaming: delta events carry no usage; the final response.completed event
// nests the complete usage under its `response` field — so both the top-level
// and the nested location are probed, and later events overwrite earlier ones.
//
// Non-streaming: the complete body is parsed in one shot.
func NewResponses() Session { return &responsesSession{} }

type responsesSession struct {
	streamingDecided bool
	isStreaming      bool
	sseBuffer        []byte // streaming: accumulates incomplete events across chunks
	bodyBuffer       []byte // non-streaming: accumulates the complete body
	usage            *domain.Usage
}

func (s *responsesSession) Feed(chunk []byte) {
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

func (s *responsesSession) Final() *domain.Usage {
	if !s.isStreaming && s.usage == nil && len(s.bodyBuffer) > 0 {
		s.tryExtract(s.bodyBuffer)
	}

	return s.usage
}

func (s *responsesSession) detectStreaming(chunk []byte) {
	s.streamingDecided = true
	trimmed := bytes.TrimLeft(chunk, " \t\r\n")
	// Responses SSE events start with an `event:` line before `data:`.
	s.isStreaming = bytes.HasPrefix(trimmed, []byte("event:")) ||
		bytes.HasPrefix(trimmed, []byte("data:")) || bytes.HasPrefix(trimmed, []byte(":"))
}

// parseSSEBuffer splits out complete events (blank-line separated, LF or CRLF)
// and tries to extract usage from each data: line's payload.
func (s *responsesSession) parseSSEBuffer() {
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

// tryExtract parses a single JSON payload: a complete Responses body (usage at
// the top level) or a streaming event (usage nested under `response`).
func (s *responsesSession) tryExtract(payload []byte) {
	var ev struct {
		Usage    *responsesUsage `json:"usage"`
		Response *struct {
			Usage *responsesUsage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return
	}

	u := ev.Usage
	if u == nil && ev.Response != nil {
		u = ev.Response.Usage
	}

	if u == nil {
		return
	}

	// Upstream returned usage exactly: source=upstream, confidence=exact. The
	// full Raw (including *_tokens_details extension fields) is stored verbatim
	// for downstream billing to parse per vendor / model rules (docs/05 §3).
	raw, _ := json.Marshal(u)
	s.usage = &domain.Usage{
		Input:      u.InputTokens,
		Output:     u.OutputTokens,
		Total:      u.TotalTokens,
		Raw:        raw,
		Source:     domain.UsageSourceUpstream,
		Confidence: domain.UsageConfidenceExact,
	}
}

type responsesUsage struct {
	InputTokens         int64           `json:"input_tokens"`
	OutputTokens        int64           `json:"output_tokens"`
	TotalTokens         int64           `json:"total_tokens"`
	InputTokensDetails  json.RawMessage `json:"input_tokens_details,omitempty"`
	OutputTokensDetails json.RawMessage `json:"output_tokens_details,omitempty"`
}
