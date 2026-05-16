package adapter

import "fmt"

// ParamSpec 描述一个 Adapter 在"协议族内部"的字段差异。
//
// 跨协议族的差异由 pkg/translator 处理；同族（如多个 OpenAI 兼容厂商）的字段名 /
// 取值范围 / 必填扩展由 ParamSpec 声明。
//
// **可选接口**：Factory 可以同时实现 ParamSpecProvider 暴露 ParamSpec；translator /
// middleware 在需要时按 type assertion 拿。没实现就视为"全字段透传，不校验"——保持
// 保守 fallback。
//
// **v0.5 范围**：只暴露数据结构（SupportedParams 白名单 + ParamMapping + ProviderExtensions
// + Validators）。**enforcement** 留给 v1.0：
//   - 三种未知参数模式（Reject / Drop / Forward）由 middleware 统一实现，按 vendor config 选
//   - Validator 链跑完才进 translator
//
// 当前 v0.5 没有 middleware 真去查 ParamSpec；定义在这里是为了：
//
//	(a) 让 OpenAI/Anthropic adapter 用代码声明各自的字段约束（生为文档生效）
//	(b) v1.0 加 enforcement 时不需要回头改 adapter
type ParamSpec struct {
	SupportedParams    map[string]struct{}       // 白名单：该上游支持的参数（成员判定，不存值）
	ParamMapping       map[string]string         // canonical 字段 → 上游字段名
	ProviderExtensions map[string]any            // 自动注入的上游专有字段
	Validators         map[string]ParamValidator // 取值范围校验 / 裁剪
}

// ParamSpecProvider Adapter 可选实现，用于声明字段差异。
type ParamSpecProvider interface {
	ParamSpec() *ParamSpec
}

// ParamValidator 单字段校验 / 裁剪。
type ParamValidator interface {
	Validate(value any) (newValue any, err error)
}

// OverflowMode RangeValidator 在越界时的行为。
type OverflowMode int

const (
	OverflowReject OverflowMode = iota // 返回错误
	OverflowClamp                      // 截断到范围内
)

// RangeValidator 数值范围校验器。
type RangeValidator struct {
	Min, Max float64
	OnOver   OverflowMode
}

// Validate 实现 ParamValidator。
func (v RangeValidator) Validate(value any) (any, error) {
	f, ok := toFloat64(value)
	if !ok {
		return nil, fmt.Errorf("RangeValidator: %v is not numeric", value)
	}
	if f >= v.Min && f <= v.Max {
		return f, nil
	}
	if v.OnOver == OverflowClamp {
		if f < v.Min {
			return v.Min, nil
		}
		return v.Max, nil
	}
	return nil, fmt.Errorf("RangeValidator: %v out of [%v, %v]", f, v.Min, v.Max)
}

func toFloat64(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint:
		return float64(x), true
	case uint32:
		return float64(x), true
	case uint64:
		return float64(x), true
	}
	return 0, false
}
