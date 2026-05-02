package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/zereker-labs/ai-gateway/pkg/adapter"
	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

func TestFactory_Metadata(t *testing.T) {
	m := Factory{}.Metadata()
	if m.Vendor != "openai" {
		t.Errorf("Vendor = %q, want openai", m.Vendor)
	}
	if m.NativeProtocol != domain.ProtoOpenAI {
		t.Errorf("NativeProtocol = %v", m.NativeProtocol)
	}
	if len(m.SupportedModalities) == 0 {
		t.Error("SupportedModalities empty")
	}
}

func TestFactory_RegisteredInRegistry(t *testing.T) {
	if f := adapter.Get("openai"); f == nil {
		t.Fatal("openai factory not registered")
	}
}

func TestSession_BuildRequest(t *testing.T) {
	ep := &domain.Endpoint{
		URL:    "https://api.openai.com/v1/chat/completions",
		APIKey: domain.Secret("sk-test"),
		Vendor: "openai",
	}
	env := &domain.RequestEnvelope{
		RawBytes: []byte(`{"model":"gpt-4o","stream":false,"messages":[]}`),
		Parsed:   domain.CanonicalRequest{Model: "gpt-4o", Stream: false},
	}
	s := newSession(context.Background(), ep, env)

	req, err := s.BuildRequest()
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if req.URL.String() != ep.URL {
		t.Errorf("URL = %s, want %s", req.URL.String(), ep.URL)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("Authorization = %q", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}

	body, _ := io.ReadAll(req.Body)
	if !bytes.Equal(body, env.RawBytes) {
		t.Errorf("body = %s, want %s", body, env.RawBytes)
	}
}

func TestSession_BuildRequest_StreamInjectsUsage(t *testing.T) {
	ep := &domain.Endpoint{URL: "u", APIKey: domain.Secret("k")}
	env := &domain.RequestEnvelope{
		RawBytes: []byte(`{"model":"gpt-4o","stream":true,"messages":[]}`),
		Parsed:   domain.CanonicalRequest{Model: "gpt-4o", Stream: true},
	}
	s := newSession(context.Background(), ep, env)

	req, _ := s.BuildRequest()
	body, _ := io.ReadAll(req.Body)

	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	so, ok := m["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options missing or wrong type: %v", m["stream_options"])
	}
	if so["include_usage"] != true {
		t.Errorf("include_usage = %v, want true", so["include_usage"])
	}
}

func TestSession_BuildRequest_StreamPreservesExistingStreamOptions(t *testing.T) {
	ep := &domain.Endpoint{URL: "u", APIKey: domain.Secret("k")}
	env := &domain.RequestEnvelope{
		RawBytes: []byte(`{"model":"gpt-4o","stream":true,"stream_options":{"other":"x"},"messages":[]}`),
		Parsed:   domain.CanonicalRequest{Model: "gpt-4o", Stream: true},
	}
	s := newSession(context.Background(), ep, env)

	req, _ := s.BuildRequest()
	body, _ := io.ReadAll(req.Body)

	var m map[string]any
	_ = json.Unmarshal(body, &m)
	so, _ := m["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Errorf("include_usage not added: %v", so)
	}
	if so["other"] != "x" {
		t.Errorf("existing field 'other' lost: %v", so)
	}
}

func TestSession_BuildRequest_NoAPIKeyOmitsHeader(t *testing.T) {
	ep := &domain.Endpoint{URL: "u"} // no APIKey
	env := &domain.RequestEnvelope{
		RawBytes: []byte(`{"model":"x"}`),
		Parsed:   domain.CanonicalRequest{Model: "x"},
	}
	s := newSession(context.Background(), ep, env)
	req, _ := s.BuildRequest()
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization should be empty: %q", got)
	}
}

func TestSession_NonStreamingFinalize(t *testing.T) {
	ep := &domain.Endpoint{URL: "u"}
	env := &domain.RequestEnvelope{
		RawBytes: []byte(`{"model":"gpt-4o","stream":false}`),
		Parsed:   domain.CanonicalRequest{Model: "gpt-4o", Stream: false},
	}
	s := newSession(context.Background(), ep, env)
	_, _ = s.BuildRequest() // sets isStreaming=false

	body := []byte(`{"id":"x","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`)
	out, err := s.Feed(body)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if !bytes.Equal(out, body) {
		t.Error("Feed should pass-through bytes")
	}

	r := s.Finalize()
	if r.Usage == nil {
		t.Fatal("Usage nil")
	}
	if r.Usage.Input != 100 || r.Usage.Output != 50 || r.Usage.Total != 150 {
		t.Errorf("Usage = %+v", r.Usage)
	}
	_ = s.Close()
}

func TestSession_NonStreamingFinalize_WithCachedTokens(t *testing.T) {
	ep := &domain.Endpoint{URL: "u"}
	env := &domain.RequestEnvelope{
		RawBytes: []byte(`{"model":"x"}`),
		Parsed:   domain.CanonicalRequest{Model: "x", Stream: false},
	}
	s := newSession(context.Background(), ep, env)
	_, _ = s.BuildRequest()

	body := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,"prompt_tokens_details":{"cached_tokens":80}}}`)
	_, _ = s.Feed(body)

	r := s.Finalize()
	if r.Usage == nil {
		t.Fatal("Usage nil")
	}
	if got := r.Usage.Details[domain.CachedInputTokens]; got != 80 {
		t.Errorf("CachedInputTokens = %d, want 80", got)
	}
}

func TestSession_StreamingFinalize_ExtractsUsageFromSSE(t *testing.T) {
	ep := &domain.Endpoint{URL: "u"}
	env := &domain.RequestEnvelope{
		RawBytes: []byte(`{"model":"gpt-4o","stream":true}`),
		Parsed:   domain.CanonicalRequest{Model: "gpt-4o", Stream: true},
	}
	s := newSession(context.Background(), ep, env)
	_, _ = s.BuildRequest() // sets isStreaming=true

	// Feed in 2 chunks to verify cross-chunk SSE parsing.
	chunk1 := []byte("data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: {\"id\":\"x\",\"choices\":[],\"usage\":")
	chunk2 := []byte("{\"prompt_tokens\":42,\"completion_tokens\":10,\"total_tokens\":52}}\n\ndata: [DONE]\n\n")

	out1, _ := s.Feed(chunk1)
	if !bytes.Equal(out1, chunk1) {
		t.Error("Feed chunk1 should pass-through")
	}
	out2, _ := s.Feed(chunk2)
	if !bytes.Equal(out2, chunk2) {
		t.Error("Feed chunk2 should pass-through")
	}

	r := s.Finalize()
	if r.Usage == nil {
		t.Fatal("Usage nil")
	}
	if r.Usage.Input != 42 || r.Usage.Output != 10 || r.Usage.Total != 52 {
		t.Errorf("Usage = %+v", r.Usage)
	}
}

func TestSession_StreamingFinalize_NoUsageReturnsNil(t *testing.T) {
	ep := &domain.Endpoint{URL: "u"}
	env := &domain.RequestEnvelope{
		RawBytes: []byte(`{"stream":true}`),
		Parsed:   domain.CanonicalRequest{Stream: true},
	}
	s := newSession(context.Background(), ep, env)
	_, _ = s.BuildRequest()

	// upstream sent no usage chunk
	_, _ = s.Feed([]byte("data: {\"choices\":[]}\n\ndata: [DONE]\n\n"))

	r := s.Finalize()
	if r.Usage != nil {
		t.Errorf("Usage should be nil; got %+v", r.Usage)
	}
}

func TestSession_FeedAfterCloseReturnsError(t *testing.T) {
	s := newSession(context.Background(), &domain.Endpoint{URL: "u"},
		&domain.RequestEnvelope{Parsed: domain.CanonicalRequest{Stream: false}})
	_, _ = s.BuildRequest()
	_ = s.Close()

	if _, err := s.Feed([]byte("x")); err == nil {
		t.Error("Feed after Close should error")
	}
}

func TestSession_CloseIdempotent(t *testing.T) {
	s := newSession(context.Background(), &domain.Endpoint{URL: "u"},
		&domain.RequestEnvelope{Parsed: domain.CanonicalRequest{}})
	_, _ = s.BuildRequest()
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestExtractDataPayload(t *testing.T) {
	cases := map[string]string{
		"data: {x}":     "{x}",
		"data:{x}":      "{x}",
		"data: [DONE]":  "[DONE]",
		":heartbeat":    "", // not a data line
		"event: ping":   "", // not a data line
		"":              "",
	}
	for line, want := range cases {
		got := extractDataPayload([]byte(line))
		if want == "" {
			if got != nil {
				t.Errorf("%q: want nil, got %q", line, got)
			}
			continue
		}
		if string(got) != want {
			t.Errorf("%q: got %q, want %q", line, got, want)
		}
	}
}

func TestEnsureStreamUsage_BadJSONUnchanged(t *testing.T) {
	bad := []byte("{not json")
	out := ensureStreamUsage(bad)
	if !bytes.Equal(out, bad) {
		t.Error("bad JSON should be returned unchanged")
	}
}

// avoid unused strings import
var _ = strings.Builder{}
