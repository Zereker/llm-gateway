package router

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// noopHandler 让 gin 把请求路由到这里，实际响应由 M7 Schedule middleware 写出；
// 跑完整条 middleware 链后才回到这里，handler 里无事可做。
func noopHandler(c *gin.Context) {}

// ReadinessChecker 一项 readiness 依赖检查（SQL ping / Redis ping）。
// cmd 装配时把 db.PingContext / redis.Ping 包成这个签名注入 Deps.Readiness。
type ReadinessChecker struct {
	Name  string
	Check func(ctx context.Context) error
}

// readyzTimeout 单项依赖检查的上限——readiness 探针本身不能慢。
const readyzTimeout = 2 * time.Second

// === 操作端点（不走主 middleware 链） ===

func registerOpsRoutes(engine *gin.Engine, checks []ReadinessChecker) {
	engine.GET("/healthz", healthzHandler)
	engine.GET("/readyz", readyzHandler(checks))
	// /metrics 直接读 prometheus default registry——pkg/metric 的 Inc/Observe/Gauge
	// 注册到那里，handler 自动暴露所有已注册的 metric。
	engine.GET("/metrics", gin.WrapH(promhttp.Handler()))
}

// healthzHandler liveness：只表示进程事件循环仍可响应，不依赖 SQL / Redis。
func healthzHandler(c *gin.Context) { c.String(200, "ok") }

// readyzHandler readiness：逐项检查注入的依赖（SQL / Redis），任一失败返 503
// ——让 k8s 摘掉这个 pod 的流量，而不是把请求灌进一个必然 503 的实例
// （docs/06 §13）。不检查 Kafka / outbox：usage 发布失败不应导致摘流量。
//
// 没有注入 checks 时退化为静态 200（测试 / 未装配场景）。
func readyzHandler(checks []ReadinessChecker) gin.HandlerFunc {
	return func(c *gin.Context) {
		for _, chk := range checks {
			ctx, cancel := context.WithTimeout(c.Request.Context(), readyzTimeout)
			err := chk.Check(ctx)
			cancel()
			if err != nil {
				c.String(503, "not ready: %s: %v", chk.Name, err)
				return
			}
		}
		c.String(200, "ok")
	}
}
