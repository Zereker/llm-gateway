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
	"github.com/tidwall/gjson"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/protocol"
)

// converseURL swaps .../converse for .../converse-stream when streaming —
// the Converse-API analog of bedrockURL (session.go), same swap logic, just
// a different suffix pair.
func converseURL(base string, streaming bool) string {
	if !streaming || strings.HasSuffix(base, "/converse-stream") {
		return base
	}
	if strings.HasSuffix(base, "/converse") {
		return base[:len(base)-len("/converse")] + "/converse-stream"
	}
	return base // non-standard URL: leave as-is
}

// converseSession handles the Converse API's HTTP layer (SigV4 signing +
// /converse vs /converse-stream). Unlike invokeSession it does no body
// rewriting -- internal/translator/openai_bedrock already produces the
// Converse-shaped body directly; the only thing this layer strips is the
// synthetic "stream" field that translator adds purely so this session can
// pick the URL without re-deriving the flag itself (see that package's doc
// comment).
type converseSession struct {
	ctx context.Context
	ep  *domain.Endpoint
	env *domain.RequestEnvelope
}

func newConverseSession(c context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) *converseSession {
	return &converseSession{ctx: c, ep: ep, env: env}
}

// BuildRequest strips the synthetic "stream" field, signs, and posts to
// /converse or /converse-stream depending on it.
func (s *converseSession) BuildRequest(body []byte, extraHeaders http.Header) (*http.Request, error) {
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

	streaming := gjson.GetBytes(body, "stream").Bool()
	reqBody, err := stripStreamField(body)
	if err != nil {
		return nil, fmt.Errorf("bedrock: strip stream field: %w", err)
	}

	req, err := http.NewRequestWithContext(s.ctx, "POST", converseURL(s.ep.Routing.URL, streaming), bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	for k, vs := range extraHeaders {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	sum := sha256.Sum256(reqBody)
	creds := awssdk.Credentials{AccessKeyID: auth.AccessKey, SecretAccessKey: auth.SecretKey}
	if err := signer.SignHTTP(s.ctx, creds, req, hex.EncodeToString(sum[:]), "bedrock", auth.Region, time.Now()); err != nil {
		return nil, fmt.Errorf("bedrock: sigv4 sign: %w", err)
	}
	return req, nil
}

func stripStreamField(body []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	delete(m, "stream")
	return json.Marshal(m)
}

// Close is an idempotent no-op.
func (s *converseSession) Close() error { return nil }

var _ protocol.Session = (*converseSession)(nil)
