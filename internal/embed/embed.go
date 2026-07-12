// Package embed provides text vectorization (Embedder) + cosine similarity — used by the
// semantic cache.
//
// Leaf package: depends only on stdlib + net/http, doesn't import middleware/respcache, so
// both can depend on it.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

// Embedder converts text into a vector. Implementations must be safe for concurrent use.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Cosine returns the cosine similarity [-1,1]; returns 0 if either vector is zero.
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}

	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}

	if na == 0 || nb == 0 {
		return 0
	}

	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// =============================================================================
// OpenAIEmbedder — calls the OpenAI-compatible /v1/embeddings endpoint
// =============================================================================

// OpenAIEmbedder calls {baseURL}/v1/embeddings (or baseURL may already be the full endpoint).
type OpenAIEmbedder struct {
	client  *http.Client
	baseURL string
	apiKey  string
	model   string
}

// NewOpenAIEmbedder constructs an OpenAIEmbedder; empty baseURL defaults to the official
// OpenAI API; empty model defaults to text-embedding-3-small.
func NewOpenAIEmbedder(apiKey, baseURL, model string) *OpenAIEmbedder {
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}

	if model == "" {
		model = "text-embedding-3-small"
	}

	return &OpenAIEmbedder{
		client:  &http.Client{Timeout: 5 * time.Second},
		baseURL: baseURL,
		apiKey:  apiKey,
		model:   model,
	}
}

func (e *OpenAIEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": e.model, "input": text})
	url := embeddingsURL(e.baseURL)

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed: upstream status %d", resp.StatusCode)
	}

	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("embed: decode: %w", err)
	}

	if len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
		return nil, errors.New("embed: empty embedding")
	}

	return out.Data[0].Embedding, nil
}

// embeddingsURL builds the /embeddings endpoint from baseURL, handling three forms:
//   - a full endpoint (.../embeddings) → used as-is
//   - already ends with /v1 (.../v1) → append /embeddings (avoids /v1/v1/embeddings)
//   - a bare host (https://host) → append /v1/embeddings
func embeddingsURL(base string) string {
	u := strings.TrimRight(base, "/")
	switch {
	case strings.HasSuffix(u, "/embeddings"):
		return u
	case strings.HasSuffix(u, "/v1"):
		return u + "/embeddings"
	default:
		return u + "/v1/embeddings"
	}
}

// Compile-time interface assertion.
var _ Embedder = (*OpenAIEmbedder)(nil)
