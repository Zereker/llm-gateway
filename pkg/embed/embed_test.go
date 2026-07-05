package embed

import (
	"math"
	"testing"
)

func TestCosine(t *testing.T) {
	cases := []struct {
		a, b []float32
		want float64
	}{
		{[]float32{1, 0}, []float32{1, 0}, 1},      // 相同
		{[]float32{1, 0}, []float32{0, 1}, 0},      // 正交
		{[]float32{1, 0}, []float32{-1, 0}, -1},    // 反向
		{[]float32{1, 1}, []float32{2, 2}, 1},      // 同向不同模
		{[]float32{1, 0}, []float32{0, 0}, 0},      // 零向量
		{[]float32{1, 0}, []float32{1, 0, 0}, 0},   // 长度不匹配
		{[]float32{}, []float32{}, 0},              // 空
	}
	for i, c := range cases {
		if got := Cosine(c.a, c.b); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("case %d: Cosine=%v want %v", i, got, c.want)
		}
	}
}

func TestEmbeddingsURL(t *testing.T) {
	cases := []struct{ base, want string }{
		{"https://api.openai.com", "https://api.openai.com/v1/embeddings"},
		{"https://api.openai.com/", "https://api.openai.com/v1/embeddings"},
		{"https://host/v1", "https://host/v1/embeddings"},          // 不能变成 /v1/v1/embeddings
		{"https://host/v1/", "https://host/v1/embeddings"},         // 尾斜杠
		{"https://host/v1/embeddings", "https://host/v1/embeddings"}, // 完整端点原样
	}
	for _, c := range cases {
		if got := embeddingsURL(c.base); got != c.want {
			t.Errorf("embeddingsURL(%q) = %q, want %q", c.base, got, c.want)
		}
	}
}
