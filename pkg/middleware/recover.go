package middleware

import (
	"log/slog"
	"runtime/debug"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/metric"
)

// Recover 是 M9：捕获 panic + 兜底写出 rc.Error。
//
// 必须紧随 M1 注册（在 c.Next() 之前），这样 defer 才能覆盖整条链。
//
// 响应 body 统一用 docs/01 §8 + docs/08 §7 的 ErrorResponse{Code,Message,Class,
// Details,RequestID,TraceID}。
func Recover() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				metric.Inc(metric.PanicTotal, "component", "middleware")
				slog.ErrorContext(c.Request.Context(), "panic recovered",
					"recover", r,
					"stack", string(debug.Stack()),
				)
				writeError(c, &domain.AdapterError{
					Class:      domain.ErrUnknown,
					Code:       domain.ErrCodeInternalError,
					HTTPStatus: 500,
					Message:    "internal server error",
				})
			}
		}()

		c.Next()

		if rc := GetRequestContext(c); rc.Error != nil && !c.Writer.Written() {
			writeError(c, rc.Error)
		}
	}
}

// writeError 按 ErrorResponse schema 写 JSON 响应。
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
