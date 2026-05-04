package extractor

import (
	"bytes"
	"encoding/json"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// NewAnthropic 构造一个 Anthropic 协议 usage Session。
//
// 适用场景（按上游协议匹配）：
//   - identity/anthropic：上游 Anthropic
//   - openai_anthropic：上游 Anthropic（OpenAI 客户端 → Anthropic 上游）
//
// **Anthropic SSE event 形态**（input_tokens 在开头，output_tokens 在结尾）：
//
//	event: message_start
//	data: {"type":"message_start","message":{...,"usage":{"input_tokens":10,"output_tokens":1}}}
//
//	event: message_delta
//	data: {"type":"message_delta","delta":{...},"usage":{"output_tokens":25}}
//
// **取值策略**：
//   - message_start.message.usage.input_tokens → input（终值；不会再变）
//   - message_delta.usage.output_tokens → output（持续 overwrite，最后一次是终值）
//
// 非流式：完整 body 顶层 usage{input_tokens, output_tokens}。
func NewAnthropic() Session { return &anthropicSession{} }

type anthropicSession struct {
	streamingDecided bool
	isStreaming      bool
	sseBuffer        []byte
	bodyBuffer       []byte

	inputTokens  int64
	outputTokens int64
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
	return &domain.Usage{
		Input:  s.inputTokens,
		Output: s.outputTokens,
		Total:  s.inputTokens + s.outputTokens,
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
		idx := bytes.Index(s.sseBuffer, []byte("\n\n"))
		if idx < 0 {
			return
		}
		event := s.sseBuffer[:idx]
		s.sseBuffer = s.sseBuffer[idx+2:]

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
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(s.bodyBuffer, &resp); err != nil {
		return
	}
	if resp.Usage != nil {
		s.inputTokens = resp.Usage.InputTokens
		s.outputTokens = resp.Usage.OutputTokens
	}
}

// tryExtract 单 SSE event payload；按 type 分发：
//
//	message_start  → message.usage.input_tokens（output_tokens 在 start 时通常是 1，跳过）
//	message_delta  → usage.output_tokens（持续 overwrite）
//
// 其它事件（content_block_*, ping, message_stop）不带 usage，跳过。
func (s *anthropicSession) tryExtract(payload []byte) {
	var ev struct {
		Type    string `json:"type"`
		Message *struct {
			Usage *struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
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
		}
	case "message_delta":
		if ev.Usage != nil && ev.Usage.OutputTokens > 0 {
			s.outputTokens = ev.Usage.OutputTokens
		}
	}
}
