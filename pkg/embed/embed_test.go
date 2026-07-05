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
