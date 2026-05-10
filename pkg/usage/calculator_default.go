package usage

import (
	"github.com/zereker/llm-gateway/pkg/domain"
)

// DefaultCalculator 默认 PriceCalculator 实现：rates × usage 各维度 × ratios。
//
// **公式**：
//
//	subtotal_<dim> = usage_<dim> × rate_<dim> / unit_size
//	cost_pre_ratio = sum(subtotal_<dim>)
//	cost           = cost_pre_ratio × model_ratio × group_ratio[user.group]
//
// 其中 unit_size 由 BaseUnit 决定：
//   - "1K_tokens"  → 1000
//   - "1_second"   → 1
//   - "1_image"    → 1
//   - 其它 / 空    → 1
//
// **覆盖维度**（按 PricingRates 字段对齐）：
//
//	usage.Input - cached_input → rates.Input
//	usage.Details[CachedInputTokens]  → rates.CachedRead
//	usage.Details[CacheCreationTokens] → rates.CachedWrite
//	usage.Output                      → rates.Output
//	usage.Details[AudioInputSeconds]  → rates.AudioSecond
//	usage.Details[VideoOutputSeconds] → rates.VideoSecond
//	usage.Details[ImageOutputCount]   → rates.ImageCount
//	usage.Details[TextCharCount]      → rates.TextChar
//
// **Reasoning tokens**：算进 Output（OpenAI o-系列定价就这么算）。
//
// **TieredPrices**：v0.5 暂不支持（接口预留，v1.0 接 CEL 同期实现）。
//
// **CEL Expression**：spec.Expression 非空时本 Calculator 应被外层路由换成 CELCalculator；
// 这里检测到非空就 panic（fail-fast 让上层装配 bug 立即暴露）；v0.5 没有 CEL 实现。
//
// 零值即可用：var c DefaultCalculator。Safe for concurrent use（无内部状态）。
type DefaultCalculator struct{}

// Calculate 实现 PriceCalculator.Calculate。
func (DefaultCalculator) Calculate(u *domain.Usage, spec *PricingSpec) (float64, []CostFormula, error) {
	if u == nil || spec == nil {
		return 0, nil, nil
	}
	if spec.Expression != "" {
		// v0.5 没 CEL；如果走到这儿说明上层装配错（应该用 CELCalculator）
		panic("usage.DefaultCalculator: spec.Expression non-empty but CEL is not implemented in v0.5; route to CELCalculator at composition site")
	}

	unit := unitSize(spec.BaseUnit)
	rates := spec.Rates

	// 提前算 cached input：从 Details 取
	var cachedInput int64
	if u.Details != nil {
		cachedInput = u.Details[domain.CachedInputTokens]
	}
	// "Input" 字段约定包含所有 cache 部分；扣掉 cached 才是按 Input rate 计费的部分
	uncachedInput := u.Input - cachedInput
	if uncachedInput < 0 {
		uncachedInput = 0
	}
	// reasoning 算进 output（OpenAI o-系列定价惯例）
	output := u.Output + u.Reasoning

	formulas := make([]CostFormula, 0, 8)
	addDim(&formulas, "input", float64(uncachedInput), spec.BaseUnit, rates.Input, unit)
	addDim(&formulas, "cached_input", float64(cachedInput), spec.BaseUnit, rates.CachedRead, unit)
	if u.Details != nil {
		addDim(&formulas, "cache_creation", float64(u.Details[domain.CacheCreationTokens]), spec.BaseUnit, rates.CachedWrite, unit)
	}
	addDim(&formulas, "output", float64(output), spec.BaseUnit, rates.Output, unit)
	if u.Details != nil {
		addDim(&formulas, "audio_input_seconds", float64(u.Details[domain.AudioInputSeconds]), "1_second", rates.AudioSecond, 1)
		addDim(&formulas, "video_output_seconds", float64(u.Details[domain.VideoOutputSeconds]), "1_second", rates.VideoSecond, 1)
		addDim(&formulas, "image_output_count", float64(u.Details[domain.ImageOutputCount]), "1_image", rates.ImageCount, 1)
		addDim(&formulas, "text_char", float64(u.Details[domain.TextCharCount]), "1_char", rates.TextChar, 1)
	}

	var preRatio float64
	for _, f := range formulas {
		preRatio += f.Subtotal
	}

	// 应用 ratios：model 全局 + group 按 user.group 选
	ratio := 1.0
	if spec.ModelRatio > 0 {
		ratio *= spec.ModelRatio
	}
	if g, ok := spec.GroupRatios[u.Meta.UserID]; ok && g > 0 {
		// 注意：roadmap 里 group ratio 按 user group（不是 user id）查；
		// v0.5 简化：UserID 当 group key 用，等 Identity 把 Group 提出来再换
		ratio *= g
	}
	cost := preRatio * ratio
	return cost, formulas, nil
}

// addDim 一个维度有 value 时加进 formulas；value=0 / rate=0 不加（减 noise）。
func addDim(out *[]CostFormula, key string, value float64, unit string, rate, unitSize float64) {
	if value <= 0 || rate <= 0 || unitSize <= 0 {
		return
	}
	subtotal := value * rate / unitSize
	*out = append(*out, CostFormula{
		Key:      key,
		Value:    value,
		Unit:     unit,
		Rate:     rate,
		Subtotal: subtotal,
	})
}

// unitSize BaseUnit → 单位大小（用于 token-per-1k 等折算）。
func unitSize(baseUnit string) float64 {
	switch baseUnit {
	case "1K_tokens", "1k_tokens":
		return 1000
	case "1M_tokens", "1m_tokens":
		return 1_000_000
	case "1_second", "1_image", "1_char", "":
		return 1
	default:
		return 1
	}
}

// 编译期断言。
var _ PriceCalculator = DefaultCalculator{}
