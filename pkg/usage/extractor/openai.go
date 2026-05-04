package extractor

import (
	"bytes"
	"encoding/json"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// NewOpenAI 构造一个 OpenAI 协议 usage Session。
//
// 适用场景（按上游协议匹配）：
//   - identity/openai：上游 OpenAI / OpenAI-compat
//   - anthropic_openai：上游 OpenAI（Anthropic 客户端 → OpenAI 上游）
//
// **OpenAI usage shape**：
//
//	{ "usage": {
//	      "prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15,
//	      "prompt_tokens_details": { "cached_tokens": 0 }
//	  } }
//
// 流式：每个 SSE event 的 data: payload 都可能含 usage；最后一个 chunk
// （include_usage=true 时）才有完整的；持续 overwrite。
//
// 非流式：完整 body 一次解析。
func NewOpenAI() Session { return &openaiSession{} }

type openaiSession struct {
	streamingDecided bool
	isStreaming      bool
	sseBuffer        []byte // 流式：跨 chunk 累计未完整事件
	bodyBuffer       []byte // 非流式：累计完整 body
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

// parseSSEBuffer 切完整事件（"\n\n" 分隔），对每个 data: 行的 payload 试取 usage。
func (s *openaiSession) parseSSEBuffer() {
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
			if bytes.Equal(payload, []byte("[DONE]")) {
				return
			}
			s.tryExtract(payload)
		}
	}
}

// tryExtract 解析单段 JSON payload（可能是 SSE event / chat 完整 body / image 完整 body）。
//
// 三种 shape 都适用：
//   - chat:  {"usage":{"prompt_tokens":N,"completion_tokens":M,"total_tokens":K, ...}}
//   - image: {"created":N, "data":[{"url":"..."}, ...]}  → 用 data 数组长度填 ImageOutputCount
//   - 都不匹配 → 跳过
func (s *openaiSession) tryExtract(payload []byte) {
	var ev struct {
		Usage *openaiUsage      `json:"usage"`
		Data  []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return
	}

	if ev.Usage != nil {
		u := &domain.Usage{
			Input:  ev.Usage.PromptTokens,
			Output: ev.Usage.CompletionTokens,
			Total:  ev.Usage.TotalTokens,
		}
		if ev.Usage.PromptTokensDetails != nil {
			if v := ev.Usage.PromptTokensDetails.CachedTokens; v > 0 {
				u.Details = map[domain.MetricKey]int64{
					domain.CachedInputTokens: v,
				}
			}
		}
		s.usage = u
		return
	}

	// 没 usage 字段：可能是 image API（无 token 概念），按 data 数组长度报 image_output_count
	if len(ev.Data) > 0 {
		s.usage = &domain.Usage{
			Details: map[domain.MetricKey]int64{
				domain.ImageOutputCount: int64(len(ev.Data)),
			},
		}
	}
}

type openaiUsage struct {
	PromptTokens        int64                  `json:"prompt_tokens"`
	CompletionTokens    int64                  `json:"completion_tokens"`
	TotalTokens         int64                  `json:"total_tokens"`
	PromptTokensDetails *openaiPromptTokDetail `json:"prompt_tokens_details"`
}

type openaiPromptTokDetail struct {
	CachedTokens int64 `json:"cached_tokens"`
}
