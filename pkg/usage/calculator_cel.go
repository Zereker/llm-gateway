package usage

import (
	"fmt"
	"sync"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// CELCalculator 用 google/cel-go 求 spec.Expression 表达式得到 cost。
//
// 表达式可访问的变量：
//
//	input         int    输入 token（uncached）
//	output        int    输出 token + reasoning
//	cached_input  int    cached_input_tokens
//	total         int    total tokens
//	rates         map    PricingRates 各字段（input / output / cached_read / ...）
//	model_ratio   double ModelRatio
//
// **示例表达式**：
//
//	"input * rates.input / 1000 + output * rates.output / 1000"
//	"(input * rates.input + output * rates.output) / 1000 * model_ratio"
//
// **缓存**：编译过的 cel.Program 按 expression string 存在 sync.Map；同表达式 N
// 次请求只编译一次。生产里 PricingSpec 通常稳定（一个 model 一个表达式）。
//
// **错误**：
//   - 表达式编译失败 → Calculate 返 err（Expression 改坏 admin 应该立刻发现）
//   - 表达式 evaluate 失败 / 类型不对 → 返 err
//   - 表达式返回非数值 → 返 err
//
// **不返 formulas**：CEL 是 black box；返 cost 即可。formulas 留 nil
// （DefaultCalculator 会返 per-dim 细分；CELCalculator 不分）。
//
// Concurrent-safe（compileCache 用 sync.Map）。
type CELCalculator struct {
	cache sync.Map // expression string → *cel.Program
	env   *cel.Env
	once  sync.Once
	envErr error
}

// NewCELCalculator 构造 CEL Calculator。env 在第一次 Calculate 时 lazy 构造。
func NewCELCalculator() *CELCalculator {
	return &CELCalculator{}
}

func (c *CELCalculator) initEnv() error {
	c.once.Do(func() {
		env, err := cel.NewEnv(
			cel.Variable("input", cel.IntType),
			cel.Variable("output", cel.IntType),
			cel.Variable("cached_input", cel.IntType),
			cel.Variable("total", cel.IntType),
			cel.Variable("model_ratio", cel.DoubleType),
			cel.Variable("rates", cel.MapType(cel.StringType, cel.DoubleType)),
		)
		if err != nil {
			c.envErr = fmt.Errorf("cel env: %w", err)
			return
		}
		c.env = env
	})
	return c.envErr
}

// Calculate 实现 PriceCalculator.Calculate。
//
// 调用前提：spec.Expression 非空（空时上层应路由到 DefaultCalculator，不该走 CEL）。
// 这里 defensive：Expression 空时也返 err 而不是静默跳过——避免装配 bug 隐蔽吞掉。
func (c *CELCalculator) Calculate(u *domain.Usage, spec *PricingSpec) (float64, []CostFormula, error) {
	if u == nil || spec == nil {
		return 0, nil, nil
	}
	if spec.Expression == "" {
		return 0, nil, fmt.Errorf("CELCalculator: spec.Expression empty (route to DefaultCalculator)")
	}
	if err := c.initEnv(); err != nil {
		return 0, nil, err
	}

	prog, err := c.compile(spec.Expression)
	if err != nil {
		return 0, nil, err
	}

	var cachedInput int64
	if u.Details != nil {
		cachedInput = u.Details[domain.CachedInputTokens]
	}
	uncachedInput := u.Input - cachedInput
	if uncachedInput < 0 {
		uncachedInput = 0
	}
	output := u.Output + u.Reasoning

	out, _, err := prog.Eval(map[string]any{
		"input":        uncachedInput,
		"output":       output,
		"cached_input": cachedInput,
		"total":        u.Total,
		"model_ratio":  spec.ModelRatio,
		"rates": map[string]float64{
			"input":        spec.Rates.Input,
			"output":       spec.Rates.Output,
			"cached_read":  spec.Rates.CachedRead,
			"cached_write": spec.Rates.CachedWrite,
			"audio_second": spec.Rates.AudioSecond,
			"video_second": spec.Rates.VideoSecond,
			"image_count":  spec.Rates.ImageCount,
			"text_char":    spec.Rates.TextChar,
		},
	})
	if err != nil {
		return 0, nil, fmt.Errorf("CEL eval %q: %w", spec.Expression, err)
	}
	cost, ok := celValueToFloat(out)
	if !ok {
		return 0, nil, fmt.Errorf("CEL %q returned non-numeric: %T", spec.Expression, out.Value())
	}
	return cost, nil, nil
}

// compile 按 expression string cache 编译结果；同表达式 N 次只编译一次。
func (c *CELCalculator) compile(expression string) (cel.Program, error) {
	if v, ok := c.cache.Load(expression); ok {
		return v.(cel.Program), nil
	}
	ast, issues := c.env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("CEL compile %q: %w", expression, issues.Err())
	}
	prog, err := c.env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("CEL program %q: %w", expression, err)
	}
	// 多 goroutine 同时编同一个表达式时竞争；用 LoadOrStore 防覆盖
	actual, _ := c.cache.LoadOrStore(expression, prog)
	return actual.(cel.Program), nil
}

func celValueToFloat(v ref.Val) (float64, bool) {
	switch x := v.(type) {
	case types.Double:
		return float64(x), true
	case types.Int:
		return float64(x), true
	}
	return 0, false
}

// 编译期断言。
var _ PriceCalculator = (*CELCalculator)(nil)

// CompositeCalculator 按 spec.Expression 是否非空路由到 CEL / Default。
//
// **装配点**：M10 Tracing 把它当 PriceCalculator 用；不需要外部判 Expression。
//
// 用法：
//
//	calc := usage.NewCompositeCalculator()
//	cost, formulas, err := calc.Calculate(usage, spec)
type CompositeCalculator struct {
	def DefaultCalculator
	cel *CELCalculator
}

// NewCompositeCalculator 构造可路由的 PriceCalculator。
func NewCompositeCalculator() *CompositeCalculator {
	return &CompositeCalculator{cel: NewCELCalculator()}
}

// Calculate 实现 PriceCalculator.Calculate。
func (c *CompositeCalculator) Calculate(u *domain.Usage, spec *PricingSpec) (float64, []CostFormula, error) {
	if spec != nil && spec.Expression != "" {
		return c.cel.Calculate(u, spec)
	}
	return c.def.Calculate(u, spec)
}

// 编译期断言。
var _ PriceCalculator = (*CompositeCalculator)(nil)
