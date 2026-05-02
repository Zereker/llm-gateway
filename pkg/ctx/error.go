package ctx

import "net/http"

// ErrorClass 错误分类。决定 RetryExecutor 的行为与默认 HTTP 状态码。
type ErrorClass int

const (
	ErrUnknown   ErrorClass = iota // 未知 / 兜底（默认 500）
	ErrInvalid                     // 客户端输入错误（400）；不重试
	ErrPermanent                   // 永久性失败（鉴权 / 配额 / 配置错误，403）；不重试，长 cooldown
	ErrTransient                   // 暂时性失败（网络 / 上游 5xx / 超时，502）；可重试
	ErrRateLimit                   // 限流（自身或上游，429）；按 cooldown 重试
)

func (c ErrorClass) String() string {
	switch c {
	case ErrInvalid:
		return "invalid"
	case ErrPermanent:
		return "permanent"
	case ErrTransient:
		return "transient"
	case ErrRateLimit:
		return "rate_limit"
	default:
		return "unknown"
	}
}

// AdapterError 网关内部统一错误结构。
//
// HTTPStatus 为 0 时由 DefaultHTTPStatus 按 Class 推导。
// Cause 是底层 error，不暴露给客户端。
type AdapterError struct {
	Class           ErrorClass
	HTTPStatus      int
	Message         string // 给客户端的简短信息
	UpstreamMessage string // 上游原始 message（debug；可选透传）
	Cause           error
}

func (e *AdapterError) Error() string { return e.Message }

func (e *AdapterError) Unwrap() error { return e.Cause }

// DefaultHTTPStatus 按 ErrorClass 给默认 HTTP 状态码。
func DefaultHTTPStatus(class ErrorClass) int {
	switch class {
	case ErrInvalid:
		return http.StatusBadRequest
	case ErrPermanent:
		return http.StatusForbidden
	case ErrRateLimit:
		return http.StatusTooManyRequests
	case ErrTransient:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}
