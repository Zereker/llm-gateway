package extractor

import (
	"bytes"
	"encoding/json"

	"github.com/zereker/llm-gateway/pkg/domain"
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
	// Preserve the cache-token dimensions in Raw so downstream billing can
	// price them (Anthropic bills cache writes/reads separately from input).
	// Total stays Input+Output — the TPM soft counter, not the cost model.
	rawMap := map[string]any{
		"input_tokens":  s.inputTokens,
		"output_tokens": s.outputTokens,
	}
	if s.cacheCreationTokens > 0 {
		rawMap["cache_creation_input_tokens"] = s.cacheCreationTokens
	}
	if s.cacheReadTokens > 0 {
		rawMap["cache_read_input_tokens"] = s.cacheReadTokens
	}
	raw, _ := json.Marshal(rawMap)
	return &domain.Usage{
		Input:      s.inputTokens,
		Output:     s.outputTokens,
		Total:      s.inputTokens + s.outputTokens,
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
		event, rest, ok := nextSSEFrame(s.sseBuffer)
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
		Usage *struct {
			InputTokens         int64 `json:"input_tokens"`
			OutputTokens        int64 `json:"output_tokens"`
			CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadTokens     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(s.bodyBuffer, &resp); err != nil {
		return
	}
	if resp.Usage != nil {
		s.inputTokens = resp.Usage.InputTokens
		s.outputTokens = resp.Usage.OutputTokens
		s.cacheCreationTokens = resp.Usage.CacheCreationTokens
		s.cacheReadTokens = resp.Usage.CacheReadTokens
	}
}

// tryExtract handles a single SSE event payload; dispatched by type:
//
//	message_start  -> message.usage.input_tokens (output_tokens at start is
//	                  usually 1, so it's skipped)
//	message_delta  -> usage.output_tokens (keeps getting overwritten)
//
// Other events (content_block_*, ping, message_stop) carry no usage, so they're
// skipped.
func (s *anthropicSession) tryExtract(payload []byte) {
	var ev struct {
		Type    string `json:"type"`
		Message *struct {
			Usage *struct {
				InputTokens         int64 `json:"input_tokens"`
				OutputTokens        int64 `json:"output_tokens"`
				CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
				CacheReadTokens     int64 `json:"cache_read_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Usage *struct {
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil && ev.Message.Usage != nil {
			s.inputTokens = ev.Message.Usage.InputTokens
			// Cache tokens are reported once, in message_start.
			s.cacheCreationTokens = ev.Message.Usage.CacheCreationTokens
			s.cacheReadTokens = ev.Message.Usage.CacheReadTokens
		}
	case "message_delta":
		if ev.Usage != nil && ev.Usage.OutputTokens > 0 {
			s.outputTokens = ev.Usage.OutputTokens
		}
	}
}
