package domain

import "net/http"

// ErrorClass 错误行为分类。决定调度层是否重试 + 默认 HTTP 状态码。
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

// 稳定机器码（docs/architecture/01 §8 + 08 §7）。
//
// `code` 是 ErrorResponse.Code 字段；客户端按 code 判断错误类型；message
// 是给人看的，不能作为程序判断依据。
const (
	ErrCodeRateLimitExceeded     = "rate_limit_exceeded"
	ErrCodeInvalidRequest        = "invalid_request"
	ErrCodeNoEndpointAvailable   = "no_endpoint_available"
	ErrCodeModelNotFound         = "model_not_found"
	ErrCodeModelNotSubscribed    = "model_not_subscribed"
	ErrCodeUnauthorized          = "unauthorized"
	ErrCodeBudgetInactive        = "budget_inactive"
	ErrCodeContentRejected       = "content_rejected"
	ErrCodeUpstreamError         = "upstream_error"
	ErrCodeInternalError         = "internal_error"
	ErrCodeDependencyUnavailable = "dependency_unavailable"
)

// AdapterError 网关内部统一错误结构。
//
// HTTPStatus 为 0 时由 DefaultHTTPStatus 按 Class 推导。
// Cause 是底层 error，不暴露给客户端。
// Code 是稳定机器码，客户端按它判断；为空时由 Class 推导默认值。
type AdapterError struct {
	Class           ErrorClass
	Code            string         // 稳定机器码；为空时由 Class 推导
	HTTPStatus      int
	Message         string         // 给客户端的简短信息
	UpstreamMessage string         // 上游原始 message（debug；可选透传）
	Details         map[string]any // 额外排障字段（限流维度 / endpoint_id 等；禁放 body / secret）
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

// DefaultCode 按 ErrorClass 推导稳定机器码（当 AdapterError.Code 为空时使用）。
func DefaultCode(class ErrorClass) string {
	switch class {
	case ErrInvalid:
		return ErrCodeInvalidRequest
	case ErrPermanent:
		return ErrCodeUnauthorized
	case ErrRateLimit:
		return ErrCodeRateLimitExceeded
	case ErrTransient:
		return ErrCodeUpstreamError
	default:
		return ErrCodeInternalError
	}
}

// ErrorResponse 网关统一的错误响应 body（docs/01 §8 + 08 §7）。
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody 错误详情。
type ErrorBody struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Class     string         `json:"class"`
	Details   map[string]any `json:"details,omitempty"`
	RequestID string         `json:"request_id"`
	TraceID   string         `json:"trace_id"`
}
