// Package cohere 是 Cohere v2 的 vendor 实现（HTTP 层）。
//
// 协议 shape 转换在 pkg/translator/openai_cohere（endpoint protocol 填 cohere）。
// 本包只管 HTTP：Bearer 鉴权 + routing.url（Cohere /v2/chat 端点）。
//
// 接入：endpoint `vendor: cohere` + `protocol: cohere` + `auth.type: bearer`
// （payload.api_key = Cohere key）。cmd/gateway blank import 本包 + openai_cohere。
package cohere

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// Factory 实现 protocol.Factory。无自定义 Classify——Cohere 错误走 DefaultClassifier
// 的 status-based 分类兜底。
type Factory struct{}

// Metadata 静态元信息。
func (Factory) Metadata() protocol.Metadata {
	return protocol.Metadata{
		Vendor:              "cohere",
		SupportedModalities: []domain.Modality{domain.ModalityChat},
	}
}

// NewSession 构造本次请求的 session。
func (Factory) NewSession(c context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) (protocol.Session, error) {
	return &session{ctx: c, ep: ep}, nil
}

func init() {
	protocol.RegisterFactory("cohere", Factory{})
}

type session struct {
	ctx context.Context
	ep  *domain.Endpoint
}

// BuildRequest：Bearer 鉴权 + routing.url。
func (s *session) BuildRequest(body []byte, extraHeaders http.Header) (*http.Request, error) {
	if s.ep.Routing.URL == "" {
		return nil, errors.New("cohere: ep.routing.url empty")
	}
	if s.ep.Auth.Type != domain.AuthTypeBearer {
		return nil, fmt.Errorf("cohere: unsupported auth type %q (want %q)", s.ep.Auth.Type, domain.AuthTypeBearer)
	}
	bearer, err := domain.DecodePayload[domain.BearerAuth](s.ep.Auth)
	if err != nil {
		return nil, fmt.Errorf("cohere: decode bearer: %w", err)
	}

	req, err := http.NewRequestWithContext(s.ctx, "POST", s.ep.Routing.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range extraHeaders { // 先 quirks
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json") // 再协议必需（覆盖）
	if bearer.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+bearer.APIKey)
	}
	return req, nil
}

// Close 幂等 no-op。
func (s *session) Close() error { return nil }

var _ protocol.Session = (*session)(nil)
