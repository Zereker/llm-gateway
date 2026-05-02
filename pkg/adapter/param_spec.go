package adapter

import "fmt"

// ParamSpec 描述一个 Adapter 在"协议族内部"的字段差异。
//
// 跨协议族的差异由 pkg/translator 处理；同族（如多个 OpenAI 兼容厂商）的字段名 /
// 取值范围 / 必填扩展由 ParamSpec 声明。
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
