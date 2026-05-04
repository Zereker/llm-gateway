package router

import (
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// noopHandler 让 gin 把请求路由到这里，实际响应由 M7 Schedule middleware 写出；
// 跑完整条 middleware 链后才回到这里，handler 里无事可做。
func noopHandler(c *gin.Context) {}

// === 操作端点（不走主 middleware 链） ===

func registerOpsRoutes(engine *gin.Engine) {
	engine.GET("/healthz", healthzHandler)
	engine.GET("/readyz", readyzHandler)
	// /metrics 直接读 prometheus default registry——pkg/metric 的 Inc/Observe/Gauge
	// 注册到那里，handler 自动暴露所有已注册的 metric。
	engine.GET("/metrics", gin.WrapH(promhttp.Handler()))
}

func healthzHandler(c *gin.Context) { c.String(200, "ok") }
func readyzHandler(c *gin.Context)  { c.String(200, "ok") }
