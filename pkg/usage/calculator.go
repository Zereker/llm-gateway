package usage

import "github.com/zereker-labs/ai-gateway/pkg/domain"

// PriceCalculator 把 Usage + PricingSpec 转换成 per-request cost。
//
// 内置：Default（rates × ratios 公式）和 CEL（用户自定义表达式覆盖）。
//
// Implementations MUST be safe for concurrent use（流处理器 / 离线 job 多 worker 同时调用）。
type PriceCalculator interface {
	Calculate(u *domain.Usage, spec *PricingSpec) (cost float64, formulas []CostFormula, err error)
}

// CostFormula 输出一个维度的细分成本（key / value / unit / rate / subtotal）。
type CostFormula struct {
	Key      string
	Value    float64
	Unit     string
	Rate     float64
	Subtotal float64
}
