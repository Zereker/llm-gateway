package embed

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

const officialOpenAIBaseURL = "https://api.openai.com"

func TestCosine(t *testing.T) {
	cases := []struct {
		a, b []float32
		want float64
	}{
		{[]float32{1, 0}, []float32{1, 0}, 1},    // identical
		{[]float32{1, 0}, []float32{0, 1}, 0},    // orthogonal
		{[]float32{1, 0}, []float32{-1, 0}, -1},  // opposite
		{[]float32{1, 1}, []float32{2, 2}, 1},    // same direction, different magnitude
		{[]float32{1, 0}, []float32{0, 0}, 0},    // zero vector
		{[]float32{1, 0}, []float32{1, 0, 0}, 0}, // length mismatch
		{[]float32{}, []float32{}, 0},            // empty
	}
	for i, c := range cases {
		if got := Cosine(c.a, c.b); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("case %d: Cosine=%v want %v", i, got, c.want)
		}
	}
}

func TestEmbeddingsURL(t *testing.T) {
	cases := []struct{ base, want string }{
		{officialOpenAIBaseURL, officialOpenAIBaseURL + "/v1/embeddings"},
		{officialOpenAIBaseURL + "/", officialOpenAIBaseURL + "/v1/embeddings"},
		{"https://host/v1", "https://host/v1/embeddings"},            // must not become /v1/v1/embeddings
		{"https://host/v1/", "https://host/v1/embeddings"},           // trailing slash
		{"https://host/v1/embeddings", "https://host/v1/embeddings"}, // full endpoint used as-is
	}
	for _, c := range cases {
		if got := embeddingsURL(c.base); got != c.want {
			t.Errorf("embeddingsURL(%q) = %q, want %q", c.base, got, c.want)
		}
	}
}

func TestOpenAIEmbedderSendsCompatibleRequest(t *testing.T) {
	embedder := NewOpenAIEmbedder("secret", "https://embedding.example/v1", "custom-model")
	embedder.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.String() != "https://embedding.example/v1/embeddings" {
			t.Fatalf("request = %s %s", req.Method, req.URL)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "custom-model" || body["input"] != "hello" {
			t.Fatalf("body = %#v", body)
		}

		return response(http.StatusOK, `{"data":[{"embedding":[1,0.5,-2]}]}`), nil
	})}

	got, err := embedder.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != 1 || got[1] != 0.5 || got[2] != -2 {
		t.Fatalf("embedding = %v", got)
	}
}

func TestOpenAIEmbedderDefaultsAndNoAuthorization(t *testing.T) {
	embedder := NewOpenAIEmbedder("", "", "")
	if embedder.baseURL != officialOpenAIBaseURL || embedder.model != "text-embedding-3-small" {
		t.Fatalf("defaults: baseURL=%q model=%q", embedder.baseURL, embedder.model)
	}
	embedder.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "" {
			t.Fatalf("unexpected authorization header")
		}
		return response(http.StatusOK, `{"data":[{"embedding":[1]}]}`), nil
	})}
	if _, err := embedder.Embed(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAIEmbedderFailures(t *testing.T) {
	transportErr := errors.New("network unavailable")
	tests := []struct {
		name      string
		baseURL   string
		transport roundTripFunc
		contains  string
	}{
		{
			name: "invalid URL", baseURL: "://bad",
			transport: func(*http.Request) (*http.Response, error) { t.Fatal("transport called"); return nil, nil },
			contains:  "missing protocol scheme",
		},
		{
			name: "transport", baseURL: "https://example.test",
			transport: func(*http.Request) (*http.Response, error) { return nil, transportErr },
			contains:  "network unavailable",
		},
		{
			name: "status", baseURL: "https://example.test",
			transport: func(*http.Request) (*http.Response, error) { return response(http.StatusTooManyRequests, `{}`), nil },
			contains:  "upstream status 429",
		},
		{
			name: "malformed JSON", baseURL: "https://example.test",
			transport: func(*http.Request) (*http.Response, error) { return response(http.StatusOK, `{`), nil },
			contains:  "decode",
		},
		{
			name: "missing data", baseURL: "https://example.test",
			transport: func(*http.Request) (*http.Response, error) { return response(http.StatusOK, `{"data":[]}`), nil },
			contains:  "empty embedding",
		},
		{
			name: "empty vector", baseURL: "https://example.test",
			transport: func(*http.Request) (*http.Response, error) {
				return response(http.StatusOK, `{"data":[{"embedding":[]}]}`), nil
			},
			contains: "empty embedding",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			embedder := NewOpenAIEmbedder("", tc.baseURL, "model")
			embedder.client = &http.Client{Transport: tc.transport}
			_, err := embedder.Embed(context.Background(), "hello")
			if err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("err = %v, want containing %q", err, tc.contains)
			}
		})
	}
}

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
