package router

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// noopHandler 让 gin 把请求路由到这里，实际响应由 M7 Schedule middleware 写出；
// 跑完整条 middleware 链后才回到这里，handler 里无事可做。
func noopHandler(c *gin.Context) {}

// === 操作端点（不走主 middleware 链） ===

func registerOpsRoutes(engine *gin.Engine) {
	engine.GET("/healthz", healthzHandler)
	engine.GET("/readyz", readyzHandler)
	engine.GET("/metrics", metricsHandler)
}

func healthzHandler(c *gin.Context) { c.String(200, "ok") }
func readyzHandler(c *gin.Context)  { c.String(200, "ok") }
func metricsHandler(c *gin.Context) {
	c.Data(200, "text/plain; version=0.0.4", []byte("# v0.1 metric endpoint stub\n"))
}

// === Pre-middleware（在 M1 之前） ===

// bodyLimitMW 限制请求体大小；超限读到 EOF 后返回 413（http.MaxBytesReader 触发）。
func bodyLimitMW(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		c.Next()
	}
}

// timeoutMW 给请求 ctx 加截止时间；上游调用与 RC.Ctx 都会感知到。
func timeoutMW(d time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), d)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
