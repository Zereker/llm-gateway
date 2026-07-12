package middleware

import (
	"log/slog"
	"runtime/debug"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/metric"
)

// Recover is M9: catches panics + falls back to writing out rc.Error.
//
// Must be registered right after M1 (before c.Next()), so its defer can cover
// the entire chain.
//
// The response body uniformly uses the ErrorResponse{Code,Message,Class,
// Details,RequestID,TraceID} shape from docs/01 §8 + docs/08 §7.
func Recover() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				metric.Inc(metric.PanicTotal, "component", "middleware")
				slog.ErrorContext(c.Request.Context(), "panic recovered",
					"recover", r,
					"stack", string(debug.Stack()),
				)
				// Only synthesize a 500 body if nothing has been sent yet. A
				// panic mid-stream (bytes already flushed to the client) must
				// not have a JSON error appended onto the in-flight response —
				// that corrupts it. Same guard as the rc.Error path below; the
				// panic is already logged + metered above.
				if !c.Writer.Written() {
					writeError(c, &domain.AdapterError{
						Class:      domain.ErrUnknown,
						Code:       domain.ErrCodeInternalError,
						HTTPStatus: 500,
						Message:    "internal server error",
					})
				}
			}
		}()

		c.Next()

		if rc := GetRequestContext(c); rc.Error != nil && !c.Writer.Written() {
			writeError(c, rc.Error)
		}
	}
}

// writeError writes a JSON response following the ErrorResponse schema.
func writeError(c *gin.Context, e *domain.AdapterError) {
	if e == nil {
		return
	}

	status := e.HTTPStatus
	if status == 0 {
		status = domain.DefaultHTTPStatus(e.Class)
	}

	code := e.Code
	if code == "" {
		code = domain.DefaultCode(e.Class)
	}

	rc := GetRequestContext(c)
	body := domain.ErrorResponse{
		Error: domain.ErrorBody{
			Code:      code,
			Message:   e.Message,
			Class:     e.Class.String(),
			Details:   e.Details,
			RequestID: rc.RequestID,
			TraceID:   TraceIDFromCtx(c.Request.Context()),
		},
	}
	c.AbortWithStatusJSON(status, body)
}
