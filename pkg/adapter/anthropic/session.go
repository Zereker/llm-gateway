package anthropic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/zereker-labs/ai-gateway/pkg/adapter"
	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/repo"
)

// **Anthropic API 必需的版本头**：admin 不需要配；adapter 自动加。
// 升级 API 版本时在这里改。
const anthropicAPIVersion = "2023-06-01"

// session **slim 版**：只管 HTTP 层（URL + auth + 必需 headers）。
//
// body 翻译 / 响应翻译 / usage 提取全在 pkg/translator/openai_anthropic.translator。
type session struct {
	ctx context.Context
	ep  *domain.Endpoint

	closed bool
}

func newSession(c context.Context, ep *domain.Endpoint) *session {
	return &session{ctx: c, ep: ep}
}

// BuildRequest 构造 *http.Request：
//   - URL: ep.Routing.URL（约定填完整 /v1/messages 端点）
//   - x-api-key: 从 ep.Auth.Payload (XAPIKeyAuth) 解码
//   - anthropic-version: hard-coded（API 必需）
//   - body: translator 已翻好（OpenAI ChatCompletion → Anthropic Messages）
//
// **vendor 校验**：本 Adapter 用 x-api-key auth；不是这个 type 直接拒（指引到正确 vendor）。
func (s *session) BuildRequest(body []byte) (*http.Request, error) {
	if s.ep.Routing.URL == "" {
		return nil, errors.New("anthropic: ep.routing.url empty")
	}
	if s.ep.Auth.Type != repo.AuthTypeXAPIKey {
		return nil, fmt.Errorf("anthropic: unsupported auth type %q (want %q)", s.ep.Auth.Type, repo.AuthTypeXAPIKey)
	}
	apikey, err := repo.DecodePayload[repo.XAPIKeyAuth](s.ep.Auth)
	if err != nil {
		return nil, fmt.Errorf("anthropic: decode x-api-key: %w", err)
	}
	if apikey.APIKey == "" {
		return nil, errors.New("anthropic: x-api-key empty")
	}

	req, err := http.NewRequestWithContext(s.ctx, "POST", s.ep.Routing.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apikey.APIKey)
	req.Header.Set("anthropic-version", anthropicAPIVersion)
	return req, nil
}

func (s *session) Close() error {
	s.closed = true
	return nil
}

// 编译期断言。
var _ adapter.Session = (*session)(nil)
