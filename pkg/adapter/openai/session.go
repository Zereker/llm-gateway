package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/zereker-labs/ai-gateway/pkg/adapter"
	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// session 是单次 OpenAI 请求的全部状态。
//
// 同协议透传：Feed 直接 echo 上游 chunk 给客户端；
// Usage 从响应中解析（流式：SSE 末尾 chunk；非流式：完整 body 的 usage 字段）。
type session struct {
	ctx context.Context
	ep  *domain.Endpoint
	env *domain.RequestEnvelope

	isStreaming bool
	sseBuffer   []byte // 跨 chunk 累计的未完整 SSE 事件
	bodyBuffer  []byte // 非流式：累计完整 body 用于 Finalize 解析
	usage       *domain.Usage
	closed      bool
}

func newSession(c context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) *session {
	return &session{ctx: c, ep: ep, env: env}
}

// BuildRequest 构造 *http.Request：
//   - URL: 直接用 ep.URL（约定填完整 chat completions 端点）
//   - Authorization: Bearer <APIKey.Reveal()>
//   - Body: 透传 RawBytes；流式时若未声明 stream_options.include_usage 则注入
//     （让上游必返回 usage chunk，否则 Finalize 取不到 Usage）
func (s *session) BuildRequest() (*http.Request, error) {
	body := s.env.RawBytes
	s.isStreaming = s.env.Parsed.Stream
	if s.isStreaming {
		body = ensureStreamUsage(body)
	}

	req, err := http.NewRequestWithContext(s.ctx, "POST", s.ep.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if k := s.ep.APIKey.Reveal(); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	return req, nil
}

// Feed 透传 chunk + 提取 usage。
//
// 流式：把 chunk 加入 SSE buffer，每凑齐一个 event 就 parse usage（不消耗事件，
// 仍透传给客户端）。
// 非流式：只累积 body buffer，Finalize 时一次解析。
func (s *session) Feed(chunk []byte) ([]byte, error) {
	if s.closed {
		return nil, errors.New("openai: session closed")
	}

	if s.isStreaming {
		s.sseBuffer = append(s.sseBuffer, chunk...)
		s.parseSSEBuffer()
	} else {
		s.bodyBuffer = append(s.bodyBuffer, chunk...)
	}

	// 同协议透传：原样返回给客户端
	return chunk, nil
}

// Finalize 返回最终 Usage（若提取成功）。
//
// 非流式：在此一次性解析完整 body 的 usage 字段。
// 流式：parseSSEBuffer 已在 Feed 中持续解析，此处直接返回。
func (s *session) Finalize() adapter.FinalizeResult {
	if !s.isStreaming && len(s.bodyBuffer) > 0 {
		s.parseFullBody()
	}
	return adapter.FinalizeResult{Usage: s.usage}
}

// Close 释放 buffer 并标记关闭；幂等。
func (s *session) Close() error {
	s.closed = true
	s.sseBuffer = nil
	s.bodyBuffer = nil
	return nil
}

// parseSSEBuffer 切出已完整的 "data: ...\n\n" 事件，对每个尝试取 usage。
func (s *session) parseSSEBuffer() {
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
			s.tryExtractUsage(payload)
		}
	}
}

// parseFullBody 直接 unmarshal 完整 JSON 找 usage。
func (s *session) parseFullBody() {
	s.tryExtractUsage(s.bodyBuffer)
}

// tryExtractUsage 解析 JSON 的 usage 字段（OpenAI standard 形态）。
func (s *session) tryExtractUsage(payload []byte) {
	var ev struct {
		Usage *openaiUsage `json:"usage"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return
	}
	if ev.Usage == nil {
		return
	}
	s.usage = &domain.Usage{
		Input:  ev.Usage.PromptTokens,
		Output: ev.Usage.CompletionTokens,
		Total:  ev.Usage.TotalTokens,
	}
	if ev.Usage.PromptTokensDetails != nil {
		if s.usage.Details == nil {
			s.usage.Details = make(map[domain.MetricKey]int64)
		}
		if v := ev.Usage.PromptTokensDetails.CachedTokens; v > 0 {
			s.usage.Details[domain.CachedInputTokens] = v
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

// extractDataPayload 从 SSE line 中提取 "data: " 后的 payload；不是 data 行返回 nil。
func extractDataPayload(line []byte) []byte {
	const prefix = "data:"
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return nil
	}
	rest := line[len(prefix):]
	// 去掉 ": " 之后可能的前导空格（SSE 规范允许 1 个空格）
	if len(rest) > 0 && rest[0] == ' ' {
		rest = rest[1:]
	}
	return bytes.TrimSpace(rest)
}

// ensureStreamUsage 在 body 中保证 stream_options.include_usage = true。
//
// 失败（JSON 不合法）时返回原 body —— Adapter 不应因这个增强失败而中止；
// 取不到 Usage 由 Finalize 返回 nil 自然兜底。
func ensureStreamUsage(body []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}

	// 解 stream_options 子对象（可能不存在 / 可能不是 object）
	var so map[string]json.RawMessage
	if raw, ok := m["stream_options"]; ok {
		_ = json.Unmarshal(raw, &so)
	}
	if so == nil {
		so = make(map[string]json.RawMessage)
	}
	so["include_usage"] = json.RawMessage(`true`)

	soBytes, err := json.Marshal(so)
	if err != nil {
		return body
	}
	m["stream_options"] = soBytes

	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}
