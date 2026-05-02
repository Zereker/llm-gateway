package middleware

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// TraceContext 是 M1：构造 *domain.RequestContext，挂到 *gin.Context。
//
// 必须最先注册（链中第一个 middleware）；否则 GetRequestContext 会 panic。
//
// 行为：
//   - X-Trace-Id header 透传客户端 trace ID；缺失则生成（"tr_<16hex>"）
//   - 生成新的 RequestID（"req_<12hex>"）
//   - 用 c.Request.Context() 作为 rc.Ctx（继承 client 断开 / timeout）
//   - rc.Logger = slog.Default().With("trace_id", traceID)；
//     后续 middleware（如 M2 Auth）可继续 With("user_id", ...) 链式追加字段
func TraceContext() gin.HandlerFunc {
	return func(c *gin.Context) {
		traceID := c.GetHeader("X-Trace-Id")
		if traceID == "" {
			traceID = genTraceID()
		}
		rc := &domain.RequestContext{
			TraceID:   traceID,
			RequestID: genRequestID(),
			StartTime: time.Now(),
			Ctx:       c.Request.Context(),
			GinCtx:    c,
			Logger:    slog.Default().With("trace_id", traceID),
			Extras:    make(map[string]any),
		}
		AttachRequestContext(c, rc)
		c.Next()
	}
}
