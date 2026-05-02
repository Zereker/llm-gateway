package usage

// PricingSpec 解码 ctx.ModelServiceSnapshot.SpecDetail JSON 后的计价规格。
//
// 详见 docs/architecture/05-metering-billing.md 第 5 节。
type PricingSpec struct {
	BaseUnit string // "1K_tokens" / "1_second" / "1_image"

	Rates PricingRates

	ModelRatio   float64
	GroupRatios  map[string]float64
	TieredPrices []PriceTier

	Expression string // CEL 表达式；非空覆盖默认 PriceCalculator
}

// PricingRates 各维度单价。
type PricingRates struct {
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

// PriceTier 阶梯价单档。
type PriceTier struct {
	Threshold int64 // input 超过此阈值切换到下一档
	Input     float64
	Output    float64
}
