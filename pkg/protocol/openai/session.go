package openai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/zereker/llm-gateway/pkg/adapter"
	"github.com/zereker/llm-gateway/pkg/domain"
)

// session **slim 版**：只管 HTTP 层（URL + auth + Content-Type）。
//
// SSE 解析 / usage 提取 / stream_options.include_usage 注入等"协议层"任务搬到
// pkg/translator/identity.openaiTranslator + openaiResponseHandler。
type session struct {
	ctx context.Context
	ep  *domain.Endpoint
	env *domain.RequestEnvelope

	closed bool
}

func newSession(c context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) *session {
	return &session{ctx: c, ep: ep, env: env}
}

// BuildRequest 构造 *http.Request：
//   - URL: ep.Routing.URL（约定填完整 chat completions 端点）
//   - Authorization: Bearer <api_key>，从 ep.Auth.Payload (BearerAuth) 解码
//   - Body: translator 已翻好（identity 透传 / 其它 translator 转协议）
//
// **vendor 校验**：本 Adapter 复用给所有 OpenAI-compatible vendor（openai/ark/
// deepseek/...），它们都用 Bearer 鉴权。所以 ep.Auth.Type 必须是 AuthTypeBearer；
// 其他类型（x-api-key 给 anthropic / aws-sigv4 给 bedrock）应该走对应专属 adapter。
func (s *session) BuildRequest(body []byte) (*http.Request, error) {
	if s.ep.Routing.URL == "" {
		return nil, errors.New("openai: ep.routing.url empty")
	}
	if s.ep.Auth.Type != domain.AuthTypeBearer {
		return nil, fmt.Errorf("openai: unsupported auth type %q (want %q)", s.ep.Auth.Type, domain.AuthTypeBearer)
	}
	bearer, err := domain.DecodePayload[domain.BearerAuth](s.ep.Auth)
	if err != nil {
		return nil, fmt.Errorf("openai: decode bearer auth: %w", err)
	}

	req, err := http.NewRequestWithContext(s.ctx, "POST", s.ep.Routing.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+bearer.APIKey)
	}
	return req, nil
}

// Close 释放资源；幂等。
func (s *session) Close() error {
	s.closed = true
	return nil
}

// 编译期断言。
var _ adapter.Session = (*session)(nil)
