package bedrock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// bedrockAnthropicVersion 是 Bedrock 上 Anthropic 模型必需的 body 字段值。
const bedrockAnthropicVersion = "bedrock-2023-05-31"

// signer 进程级共享——v4.Signer 无 per-call 可变状态，SignHTTP 并发安全。
var signer = v4.NewSigner()

// session 管 Bedrock 的 HTTP 层（body 改写 + SigV4 签名）。协议 shape 复用 Anthropic。
type session struct {
	ctx context.Context
	ep  *domain.Endpoint
	env *domain.RequestEnvelope
}

func newSession(c context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) *session {
	return &session{ctx: c, ep: ep, env: env}
}

// BuildRequest 改写 body（Anthropic→Bedrock）后 SigV4 签名。
func (s *session) BuildRequest(body []byte, extraHeaders http.Header) (*http.Request, error) {
	if s.ep.Routing.URL == "" {
		return nil, errors.New("bedrock: ep.routing.url empty")
	}
	if s.ep.Auth.Type != domain.AuthTypeAWSSigV4 {
		return nil, fmt.Errorf("bedrock: unsupported auth type %q (want %q)", s.ep.Auth.Type, domain.AuthTypeAWSSigV4)
	}
	auth, err := domain.DecodePayload[domain.AWSSigV4Auth](s.ep.Auth)
	if err != nil {
		return nil, fmt.Errorf("bedrock: decode aws-sigv4 auth: %w", err)
	}
	if auth.AccessKey == "" || auth.SecretKey == "" || auth.Region == "" {
		return nil, errors.New("bedrock: aws-sigv4 auth needs access_key / secret_key / region")
	}

	reqBody, err := toBedrockBody(body)
	if err != nil {
		return nil, fmt.Errorf("bedrock: rewrite body: %w", err)
	}

	req, err := http.NewRequestWithContext(s.ctx, "POST", s.ep.Routing.URL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	// 先 quirks header，再协议必需 header（覆盖）。
	for k, vs := range extraHeaders {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	// SigV4 签名（官方 signer）。payloadHash = hex(sha256(body))。
	sum := sha256.Sum256(reqBody)
	creds := awssdk.Credentials{AccessKeyID: auth.AccessKey, SecretAccessKey: auth.SecretKey}
	if err := signer.SignHTTP(s.ctx, creds, req, hex.EncodeToString(sum[:]), "bedrock", auth.Region, time.Now()); err != nil {
		return nil, fmt.Errorf("bedrock: sigv4 sign: %w", err)
	}
	return req, nil
}

// toBedrockBody 把 Anthropic Messages body 改成 Bedrock InvokeModel body：
//   - 删顶层 model（modelId 在 URL 里）
//   - 补 anthropic_version（Bedrock 必需）
//   - 删 stream（v1 只做非流式 InvokeModel）
//
// 用 map[string]RawMessage 保留其余字段原值，不重解析 messages/tools。
func toBedrockBody(body []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	delete(m, "model")
	delete(m, "stream")
	if _, ok := m["anthropic_version"]; !ok {
		m["anthropic_version"] = json.RawMessage(`"` + bedrockAnthropicVersion + `"`)
	}
	return json.Marshal(m)
}

// Close 幂等 no-op。
func (s *session) Close() error { return nil }

// 编译期断言。
var _ protocol.Session = (*session)(nil)
