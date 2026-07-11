package domain

import (
	"net/http"

	"github.com/zereker/llm-gateway/internal/failure"
)

// ErrorClass classifies error behavior. Determines whether the scheduling
// layer retries + the default HTTP status code.
type ErrorClass = failure.Class

const (
	ErrUnknown   = failure.Unknown
	ErrInvalid   = failure.Invalid
	ErrPermanent = failure.Permanent
	ErrTransient = failure.Transient
	ErrRateLimit = failure.Capacity
)

// Stable machine codes (docs/architecture/01 §8 + 08 §7).
//
// `code` is the ErrorResponse.Code field; clients determine the error type by
// code; message is for humans and must not be used as a programmatic
// decision basis.
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
	ErrCodeClientClosedRequest   = "client_closed_request"
)

// AdapterError is the gateway's unified internal error structure.
//
// When HTTPStatus is 0, it's derived from Class via DefaultHTTPStatus.
// Cause is the underlying error, not exposed to the client.
// Code is the stable machine code that clients key off of; when empty, its
// default is derived from Class.
type AdapterError struct {
	Class           ErrorClass
	Code            string // stable machine code; derived from Class when empty
	HTTPStatus      int
	Message         string         // short message for the client
	UpstreamMessage string         // raw upstream message (debug; optionally passed through)
	Details         map[string]any // extra troubleshooting fields (rate-limit dimension / endpoint_id / etc.; never put body / secrets here)
	Cause           error
}

func (e *AdapterError) Error() string { return e.Message }

func (e *AdapterError) Unwrap() error { return e.Cause }

// DefaultHTTPStatus derives the default HTTP status code from ErrorClass.
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

// DefaultCode derives the stable machine code from ErrorClass (used when AdapterError.Code is empty).
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

// ErrorResponse is the gateway's unified error response body (docs/01 §8 + 08 §7).
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody holds error details.
type ErrorBody struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Class     string         `json:"class"`
	Details   map[string]any `json:"details,omitempty"`
	RequestID string         `json:"request_id"`
	TraceID   string         `json:"trace_id"`
}
