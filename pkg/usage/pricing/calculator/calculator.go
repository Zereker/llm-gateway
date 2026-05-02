// Package calculator 实现 PricingSpec → cost 的计算（默认公式 + CEL）。
//
// TODO: Default 实现 + CEL 实现见 step 5+。
package calculator

import (
	"github.com/zereker-labs/ai-gateway/pkg/ctx"
	"github.com/zereker-labs/ai-gateway/pkg/usage/pricing"
)

// Calculator 把 Usage + PricingSpec 转换成 per-request cost。
type Calculator interface {
	Calculate(u *ctx.Usage, spec *pricing.PricingSpec) (cost float64, formulas []Formula, err error)
}

// Formula 输出一个维度的细分成本（key / value / unit / rate / subtotal）。
type Formula struct {
	Key      string
	Value    float64
	Unit     string
	Rate     float64
	Subtotal float64
}
