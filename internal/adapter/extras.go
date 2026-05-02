package adapter

import (
	"github.com/zereker-labs/ai-gateway/internal/envelope"
	"github.com/zereker-labs/ai-gateway/internal/errs"
)

// === ParamSpec：同协议族字段适配（可选实现） ===

// ParamSpec 描述一个 Adapter 在"协议族内部"的字段差异。
//
// 跨协议族的差异由 Translator 处理；同族（如多个 OpenAI 兼容厂商）的字段名 /
// 取值范围 / 必填扩展由 ParamSpec 声明。
type ParamSpec struct {
	SupportedParams    map[string]bool           // 白名单：该上游支持的参数
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
	Reject OverflowMode = iota // 返回错误
	Clamp                      // 截断到范围内
)

// RangeValidator 数值范围校验器。
type RangeValidator struct {
	Min, Max float64
	OnOver   OverflowMode
}

// === Classifier：上游错误分类（可选实现） ===

// Classifier 把上游 HTTP 状态 + body 映射到 errs.Error。
//
// Adapter 可实现 Classifier 接管特定厂商的 error schema；未实现走 DefaultClassifier。
type Classifier interface {
	Classify(httpStatus int, body []byte) *errs.Error
}

// DefaultClassifier 仅按 HTTP 状态分类。
type DefaultClassifier struct{}

func (DefaultClassifier) Classify(httpStatus int, body []byte) *errs.Error {
	e := &errs.Error{
		HTTPStatus:      httpStatus,
		UpstreamMessage: string(body),
	}
	switch {
	case httpStatus == 429:
		e.Class = errs.RateLimit
	case httpStatus == 401, httpStatus == 403:
		e.Class = errs.Permanent
	case httpStatus >= 400 && httpStatus < 500:
		e.Class = errs.Invalid
	case httpStatus >= 500:
		e.Class = errs.Transient
	default:
		e.Class = errs.Unknown
	}
	return e
}

// === CapabilityProvider：模型能力声明（可选实现，本期不强制） ===

// ModelCapabilities 模型对外暴露的能力。
type ModelCapabilities struct {
	MaxContextTokens      int
	SupportsThinking      bool
	SupportsTools         bool
	SupportsVision        bool
	SupportsStructuredOut bool
	SupportsMultiTurn     bool
	MaxToolCalls          int
	SupportedModalities   []envelope.Modality
}

// CapabilityProvider Adapter 可选实现。端点选择层在"按能力过滤"时调用。
type CapabilityProvider interface {
	ModelCapabilities(model string) *ModelCapabilities
}
