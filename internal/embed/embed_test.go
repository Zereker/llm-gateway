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
		{"https://api.openai.com", "https://api.openai.com/v1/embeddings"},
		{"https://api.openai.com/", "https://api.openai.com/v1/embeddings"},
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
