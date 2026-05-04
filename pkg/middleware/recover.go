package middleware

import (
	"log/slog"
	"runtime/debug"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/metric"
)

// Recover 是 M9：捕获 panic + 兜底写出 rc.Error。
//
// 必须紧随 M1 注册（在 c.Next() 之前），这样 defer 才能覆盖整条链。
//
// 处理两类终态：
//  1. defer recover()：M2-M8 中任何 panic 都被捕获，写出 500
//  2. c.Next() 之后：若 rc.Error != nil 且响应未写出（如 M7 RetryExecutor 失败），
//     按 errs.Class 推默认 HTTP 状态写出
func Recover() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				metric.Inc(metric.PanicTotal, "component", "middleware")
				// 用带 ctx 的 log API：trace.CtxHandler 自动从 ctx 抽 trace_id /
				// span_id / request_id / user_id 加进 record。
				ctx := c.Request.Context() // fallback：没 RC 时用 gin 的原 ctx
				if rc := TryGetRequestContext(c); rc != nil && rc.Ctx != nil {
					ctx = rc.Ctx
				}
				slog.ErrorContext(ctx, "panic recovered",
					"recover", r,
					"stack", string(debug.Stack()),
				)
				writeError(c, &domain.AdapterError{
					Class:      domain.ErrUnknown,
					HTTPStatus: 500,
					Message:    "internal server error",
				})
			}
		}()

		c.Next()

		if rc := TryGetRequestContext(c); rc != nil && rc.Error != nil && !c.Writer.Written() {
			writeError(c, rc.Error)
		}
	}
}

// writeError 按 AdapterError 写 JSON 响应。
//
// 若 e.HTTPStatus 为 0，按 e.Class 取默认状态码。
// request_id / trace_id 在 RequestContext 已就绪时一并放进响应体，便于客户端反馈定位。
func writeError(c *gin.Context, e *domain.AdapterError) {
	if e == nil {
		return
	}
	status := e.HTTPStatus
	if status == 0 {
		status = domain.DefaultHTTPStatus(e.Class)
	}
	errBody := gin.H{
		"code":    e.Class.String(),
		"message": e.Message,
	}
	if rc := TryGetRequestContext(c); rc != nil {
		errBody["request_id"] = rc.RequestID
		errBody["trace_id"] = TraceIDFromCtx(rc.Ctx)
	}
	c.AbortWithStatusJSON(status, gin.H{"error": errBody})
}
