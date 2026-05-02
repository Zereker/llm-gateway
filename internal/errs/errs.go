// Package errs 定义网关内部统一的错误模型：Class 分类与 Error 结构。
//
// 所有上游 / 内部错误最终都归一到 *Error，由 errs.Class 决定重试 / cooldown / HTTP 状态。
package errs

// Class 错误分类。决定 RetryExecutor 的行为与默认 HTTP 状态码。
type Class int

const (
	Unknown   Class = iota // 未知 / 兜底（默认 500）
	Invalid                // 客户端输入错误（400）；不重试
	Permanent              // 永久性失败（鉴权 / 配额 / 配置错误，403）；不重试，长 cooldown
	Transient              // 暂时性失败（网络 / 上游 5xx / 超时，502）；可重试
	RateLimit              // 限流（自身或上游，429）；按 cooldown 重试
)

func (c Class) String() string {
	switch c {
	case Invalid:
		return "invalid"
	case Permanent:
		return "permanent"
	case Transient:
		return "transient"
	case RateLimit:
		return "rate_limit"
	default:
		return "unknown"
	}
}

// Error 网关内部统一错误结构。
//
// HTTPStatus 为 0 时由 DefaultHTTPStatus 按 Class 推导。
// Cause 是底层 error，不暴露给客户端。
type Error struct {
	Class           Class
	HTTPStatus      int
	Message         string // 给客户端的简短信息
	UpstreamMessage string // 上游原始 message（debug；可选透传）
	Cause           error
}

func (e *Error) Error() string { return e.Message }

func (e *Error) Unwrap() error { return e.Cause }

// DefaultHTTPStatus 按 Class 给默认 HTTP 状态码。
func DefaultHTTPStatus(class Class) int {
	switch class {
	case Invalid:
		return 400
	case Permanent:
		return 403
	case RateLimit:
		return 429
	case Transient:
		return 502
	default:
		return 500
	}
}
