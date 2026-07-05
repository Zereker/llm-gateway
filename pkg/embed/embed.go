// Package embed 提供文本向量化（Embedder）+ 余弦相似度——语义缓存用。
//
// 叶子包:只依赖 stdlib + net/http,不引 middleware/respcache,供两者共用。
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

// Embedder 把文本转成向量。实现须并发安全。
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Cosine 余弦相似度 [-1,1]；任一零向量返 0。
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
// OpenAIEmbedder — 调 OpenAI-compatible /v1/embeddings
// =============================================================================

// OpenAIEmbedder 调 {baseURL}/v1/embeddings（或 baseURL 直接是完整端点）。
type OpenAIEmbedder struct {
	client  *http.Client
	baseURL string
	apiKey  string
	model   string
}

// NewOpenAIEmbedder 构造；baseURL 空 = OpenAI 官方；model 空 = text-embedding-3-small。
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

// embeddingsURL 由 baseURL 拼出 /embeddings 端点,兼容三种写法:
//   - 完整端点（.../embeddings）→ 原样
//   - 已带 /v1（.../v1）→ 补 /embeddings（避免出现 /v1/v1/embeddings）
//   - 裸 host（https://host）→ 补 /v1/embeddings
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

// 编译期断言。
var _ Embedder = (*OpenAIEmbedder)(nil)
