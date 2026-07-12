package extractor

import (
	"bytes"
	"encoding/json"

	"github.com/zereker/llm-gateway/internal/domain"
)

// NewAnthropic constructs a usage Session for the Anthropic protocol.
//
// Applicable scenarios (matched by upstream protocol):
//   - identity/anthropic: upstream is Anthropic
//   - openai_anthropic: upstream is Anthropic (OpenAI client -> Anthropic upstream)
//
// **Anthropic SSE event shape** (input_tokens at the start, output_tokens at the
// end):
//
//	event: message_start
//	data: {"type":"message_start","message":{...,"usage":{"input_tokens":10,"output_tokens":1}}}
//
//	event: message_delta
//	data: {"type":"message_delta","delta":{...},"usage":{"output_tokens":25}}
//
// **Value strategy**:
//   - message_start.message.usage.input_tokens -> input (final value; never changes
//     again)
//   - message_delta.usage.output_tokens -> output (keeps getting overwritten; the
//     last one is the final value)
//
// Non-streaming: the complete body's top-level usage{input_tokens, output_tokens}.
func NewAnthropic() Session { return &anthropicSession{} }

type anthropicSession struct {
	streamingDecided bool
	isStreaming      bool
	sseBuffer        []byte
	bodyBuffer       []byte

	inputTokens         int64
	outputTokens        int64
	cacheCreationTokens int64 // cache_creation_input_tokens (billed separately)
	cacheReadTokens     int64 // cache_read_input_tokens (billed separately)

	// rawUsage accumulates the verbatim upstream usage fields (streaming:
	// merged from message_start's usage + message_delta's usage; non-streaming:
	// the whole usage object) so downstream billing can price dimensions the
	// gateway doesn't model itself — e.g. cache_creation.ephemeral_1h_input_tokens
	// vs ephemeral_5m_input_tokens bill at different multipliers, and
	// service_tier affects price too (docs/architecture/05-metering-billing.md §3).
	rawUsage map[string]json.RawMessage
}

func (s *anthropicSession) Feed(chunk []byte) {
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

func (s *anthropicSession) Final() *domain.Usage {
	if !s.isStreaming && s.inputTokens == 0 && s.outputTokens == 0 && len(s.bodyBuffer) > 0 {
		s.parseFullBody()
	}

	if s.inputTokens == 0 && s.outputTokens == 0 {
		return nil
	}
	// Total mirrors what Anthropic's own ITPM/OTPM rate limits actually count,
	// not the cost model: cache_creation_input_tokens counts toward ITPM (a
	// cache write is billed at a premium, but it's still tokens Anthropic had
	// to process), while cache_read_input_tokens does not (for all current
	// models except Haiku 3.5) — so it stays excluded. Getting this wrong in
	// either direction means our internal TPM bucket diverges from what
	// actually throttles the account upstream, either double-guarding against
	// throughput Anthropic already allows for free, or letting a cache-heavy
	// account look "under quota" here while it isn't upstream.
	// Raw is the verbatim upstream usage object (see rawUsage's doc comment).
	raw, _ := json.Marshal(s.rawUsage)

	return &domain.Usage{
		Input:      s.inputTokens,
		Output:     s.outputTokens,
		Total:      s.inputTokens + s.cacheCreationTokens + s.outputTokens,
		Raw:        raw,
		Source:     domain.UsageSourceUpstream,
		Confidence: domain.UsageConfidenceExact,
	}
}

func (s *anthropicSession) detectStreaming(chunk []byte) {
	s.streamingDecided = true
	trimmed := bytes.TrimLeft(chunk, " \t\r\n")
	s.isStreaming = bytes.HasPrefix(trimmed, []byte("data:")) ||
		bytes.HasPrefix(trimmed, []byte("event:")) ||
		bytes.HasPrefix(trimmed, []byte(":"))
}

func (s *anthropicSession) parseSSEBuffer() {
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

			s.tryExtract(payload)
		}
	}
}

func (s *anthropicSession) parseFullBody() {
	var resp struct {
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(s.bodyBuffer, &resp); err != nil {
		return
	}

	if len(resp.Usage) == 0 {
		return
	}

	var counts struct {
		InputTokens         int64 `json:"input_tokens"`
		OutputTokens        int64 `json:"output_tokens"`
		CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	}
	if err := json.Unmarshal(resp.Usage, &counts); err != nil {
		return
	}

	s.inputTokens = counts.InputTokens
	s.outputTokens = counts.OutputTokens
	s.cacheCreationTokens = counts.CacheCreationTokens
	s.cacheReadTokens = counts.CacheReadTokens

	var m map[string]json.RawMessage
	if err := json.Unmarshal(resp.Usage, &m); err == nil {
		s.rawUsage = m
	}
}

// tryExtract handles a single SSE event payload; dispatched by type:
//
//	message_start  -> message.usage.input_tokens (output_tokens at start is
//	                  usually 1, so it's skipped)
//	message_delta  -> usage.output_tokens (keeps getting overwritten); some
//	                  anthropic-compatible vendors report input_tokens 0 in
//	                  message_start and ship the full usage in message_delta
//	                  instead — so a non-zero input_tokens here overwrites too
//
// Other events (content_block_*, ping, message_stop) carry no usage, so they're
// skipped.
func (s *anthropicSession) tryExtract(payload []byte) {
	var ev struct {
		Type    string `json:"type"`
		Message *struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"message"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return
	}

	var usageRaw json.RawMessage
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			usageRaw = ev.Message.Usage
		}
	case "message_delta":
		usageRaw = ev.Usage
	default:
		return
	}

	if len(usageRaw) == 0 {
		return
	}

	var counts struct {
		InputTokens         int64 `json:"input_tokens"`
		OutputTokens        int64 `json:"output_tokens"`
		CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	}
	if err := json.Unmarshal(usageRaw, &counts); err != nil {
		return
	}

	switch ev.Type {
	case "message_start":
		s.inputTokens = counts.InputTokens
		// Cache tokens are reported once, in message_start.
		s.cacheCreationTokens = counts.CacheCreationTokens
		s.cacheReadTokens = counts.CacheReadTokens

		var m map[string]json.RawMessage
		if err := json.Unmarshal(usageRaw, &m); err == nil {
			s.rawUsage = m
		}
	case "message_delta":
		// Some anthropic-compatible vendors report input_tokens 0 in
		// message_start and ship the full usage in message_delta instead —
		// so a non-zero input_tokens here overwrites too.
		if counts.OutputTokens > 0 {
			s.outputTokens = counts.OutputTokens
		}

		if counts.InputTokens > 0 {
			s.inputTokens = counts.InputTokens
		}

		if counts.OutputTokens > 0 || counts.InputTokens > 0 {
			var m map[string]json.RawMessage
			if err := json.Unmarshal(usageRaw, &m); err == nil {
				if s.rawUsage == nil {
					s.rawUsage = map[string]json.RawMessage{}
				}

				for k, v := range m {
					s.rawUsage[k] = v
				}
			}
		}
	}
}
