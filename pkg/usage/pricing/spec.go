// Package pricing 定义计价规格 PricingSpec 与版本化机制。
//
// PricingSpec 存储在 ctx.ModelServiceSnapshot.SpecDetail JSON 字段中；
// 详见 docs/architecture/05-metering-billing.md 第 5 节。
package pricing

// PricingSpec 解码 ModelServiceSnapshot.SpecDetail JSON 后的计价规格。
type PricingSpec struct {
	BaseUnit string // "1K_tokens" / "1_second" / "1_image"

	Rates Rates

	ModelRatio   float64
	GroupRatios  map[string]float64
	TieredPrices []TierStop

	Expression string // CEL 表达式；非空覆盖默认 Calculator
}

// Rates 各维度单价。
type Rates struct {
	Input       float64
	Output      float64
	CachedRead  float64
	CachedWrite float64
	AudioSecond float64
	VideoSecond float64
	ImageCount  float64
	TextChar    float64
	// 新维度按需扩展
}

// TierStop 阶梯价单档。
type TierStop struct {
	Threshold int64 // input 超过此阈值切换到下一档
	Input     float64
	Output    float64
}
