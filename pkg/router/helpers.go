package router

import (
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
