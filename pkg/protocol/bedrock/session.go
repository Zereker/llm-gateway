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
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/tidwall/gjson"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// bedrockURL swaps .../invoke for .../invoke-with-response-stream when streaming.
func bedrockURL(base string, streaming bool) string {
	if !streaming || strings.HasSuffix(base, "/invoke-with-response-stream") {
		return base
	}
	if strings.HasSuffix(base, "/invoke") {
		return base[:len(base)-len("/invoke")] + "/invoke-with-response-stream"
	}
	return base // non-standard URL: leave as-is (deployer may already point at the streaming endpoint)
}

// bedrockAnthropicVersion is the required body field value for Anthropic models on Bedrock.
const bedrockAnthropicVersion = "bedrock-2023-05-31"

// signer is shared process-wide — v4.Signer has no per-call mutable state, so SignHTTP is concurrency-safe.
var signer = v4.NewSigner()

// session handles Bedrock's HTTP layer (body rewriting + SigV4 signing). The protocol shape is reused from Anthropic.
type session struct {
	ctx context.Context
	ep  *domain.Endpoint
	env *domain.RequestEnvelope
}

func newSession(c context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) *session {
	return &session{ctx: c, ep: ep, env: env}
}

// BuildRequest rewrites the body (Anthropic→Bedrock) and then applies a SigV4 signature.
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

	// Client wants streaming → use the invoke-with-response-stream endpoint (the
	// response is AWS event-stream, decoded into Anthropic SSE by
	// Factory.DecodeTransport). The stream flag is stripped during the rewrite below.
	streaming := gjson.GetBytes(body, "stream").Bool()
	reqBody, err := toBedrockBody(body)
	if err != nil {
		return nil, fmt.Errorf("bedrock: rewrite body: %w", err)
	}

	req, err := http.NewRequestWithContext(s.ctx, "POST", bedrockURL(s.ep.Routing.URL, streaming), bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	// Quirks headers first, then protocol-required headers (which override them).
	for k, vs := range extraHeaders {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	// SigV4 signature (official signer). payloadHash = hex(sha256(body)).
	sum := sha256.Sum256(reqBody)
	creds := awssdk.Credentials{AccessKeyID: auth.AccessKey, SecretAccessKey: auth.SecretKey}
	if err := signer.SignHTTP(s.ctx, creds, req, hex.EncodeToString(sum[:]), "bedrock", auth.Region, time.Now()); err != nil {
		return nil, fmt.Errorf("bedrock: sigv4 sign: %w", err)
	}
	return req, nil
}

// toBedrockBody turns an Anthropic Messages body into a Bedrock InvokeModel body:
//   - removes the top-level model field (modelId is already in the URL)
//   - fills in anthropic_version (required by Bedrock)
//   - removes stream (v1 only does non-streaming InvokeModel)
//
// Uses map[string]RawMessage to preserve the other fields' original values without
// re-parsing messages/tools.
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

// Close is an idempotent no-op.
func (s *session) Close() error { return nil }

// Compile-time assertion.
var _ protocol.Session = (*session)(nil)
