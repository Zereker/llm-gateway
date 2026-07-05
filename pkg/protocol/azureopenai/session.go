package azureopenai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// session 只管 Azure OpenAI 的 HTTP 层（URL + api-version + api-key 头）。
// 协议 shape（SSE 解析 / usage 提取）复用 OpenAI 的 translator + response handler。
type session struct {
	ctx context.Context
	ep  *domain.Endpoint
	env *domain.RequestEnvelope
}

func newSession(c context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) *session {
	return &session{ctx: c, ep: ep, env: env}
}

// BuildRequest 构造 *http.Request：
//   - URL: ep.Routing.URL（完整 Azure 端点）；若缺 api-version query 且
//     ep.Routing.APIVersion 非空，则补上。
//   - 鉴权：`api-key: <key>`（复用 AuthTypeBearer 的 BearerAuth.APIKey 载荷）。
//   - header 顺序：先 quirks，再协议必需 header（覆盖），防 deployer 误改鉴权头。
func (s *session) BuildRequest(body []byte, extraHeaders http.Header) (*http.Request, error) {
	if s.ep.Routing.URL == "" {
		return nil, errors.New("azure-openai: ep.routing.url empty")
	}
	if s.ep.Auth.Type != domain.AuthTypeBearer {
		return nil, fmt.Errorf("azure-openai: unsupported auth type %q (want %q; payload.api_key = Azure key)",
			s.ep.Auth.Type, domain.AuthTypeBearer)
	}
	key, err := domain.DecodePayload[domain.BearerAuth](s.ep.Auth)
	if err != nil {
		return nil, fmt.Errorf("azure-openai: decode auth: %w", err)
	}

	endpoint, err := ensureAPIVersion(s.ep.Routing.URL, s.ep.Routing.APIVersion)
	if err != nil {
		return nil, fmt.Errorf("azure-openai: routing url: %w", err)
	}

	req, err := http.NewRequestWithContext(s.ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range extraHeaders { // 先 quirks
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json") // 再协议必需（覆盖）
	if key.APIKey != "" {
		req.Header.Set("api-key", key.APIKey)
	}
	return req, nil
}

// ensureAPIVersion 若 URL 缺 api-version query 且提供了 version，则补上。
func ensureAPIVersion(raw, version string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if q.Get("api-version") == "" && version != "" {
		q.Set("api-version", version)
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// Close 幂等 no-op（本 session 不持资源）。
func (s *session) Close() error { return nil }

// 编译期断言。
var _ protocol.Session = (*session)(nil)
