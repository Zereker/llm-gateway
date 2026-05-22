package gemini

import (
	"bytes"
	"context"
	"errors"
	"net/http"

	"github.com/zereker/llm-gateway/pkg/protocol"
	"github.com/zereker/llm-gateway/pkg/domain"
)

// session **slim 版**：只管 HTTP 层（URL + auth header）。
//
// body 翻译 / 响应翻译 / usage 提取全在 pkg/translator/openai_gemini.translator。
//
// auth 三种形态由 newTokenProvider 统一抽象（gemini-key / vertex-adc / oauth2-sa）；
// session 不知道具体类型。
type session struct {
	ctx context.Context
	ep  *domain.Endpoint
	tp  tokenProvider

	closed bool
}

func newSession(c context.Context, ep *domain.Endpoint, tp tokenProvider) *session {
	return &session{ctx: c, ep: ep, tp: tp}
}

// BuildRequest 构造 *http.Request：
//   - URL: ep.Routing.URL（约定填完整 :generateContent 端点）
//   - 加 vendor-specific auth header（x-goog-api-key 或 Authorization: Bearer）
//   - body: translator 已翻好（OpenAI ChatCompletion → Gemini generateContent）
//
// **流式**：v0.5 不支持。客户端发 stream=true 时 openai_gemini translator 在
// TranslateRequest 阶段会返错（adapter 不会被调到）。
func (s *session) BuildRequest(body []byte, extraHeaders http.Header) (*http.Request, error) {
	if s.ep.Routing.URL == "" {
		return nil, errors.New("gemini: ep.routing.url empty")
	}
	req, err := http.NewRequestWithContext(s.ctx, "POST", s.ep.Routing.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// 先 quirks header
	for k, vs := range extraHeaders {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// 再协议必需 header（覆盖）
	req.Header.Set("Content-Type", "application/json")

	hdrName, hdrValue, err := s.tp.AuthHeader(s.ctx)
	if err != nil {
		return nil, err
	}
	req.Header.Set(hdrName, hdrValue)

	return req, nil
}

func (s *session) Close() error {
	s.closed = true
	return nil
}

// 编译期断言。
var _ protocol.Session = (*session)(nil)
