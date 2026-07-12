package router

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// noopHandler lets gin route the request here, but the actual response is
// written by the M7 Schedule middleware; by the time control reaches this
// handler the whole middleware chain has already run, so there's nothing
// left to do.
func noopHandler(c *gin.Context) {}

// ReadinessChecker is a single readiness dependency check (SQL ping / Redis
// ping). During cmd assembly, db.PingContext / redis.Ping are wrapped into
// this signature and injected into Deps.Readiness.
type ReadinessChecker struct {
	Name  string
	Check func(ctx context.Context) error
}

// readyzTimeout is the cap for a single dependency check — the readiness
// probe itself must not be slow.
const readyzTimeout = 2 * time.Second

// === Ops endpoints (bypass the main middleware chain) ===

func registerOpsRoutes(engine *gin.Engine, checks []ReadinessChecker) {
	engine.GET("/healthz", healthzHandler)
	engine.GET("/readyz", readyzHandler(checks))
	// /metrics reads directly from the prometheus default registry — internal/metric's
	// Inc/Observe/Gauge register there, so the handler automatically exposes
	// every registered metric.
	engine.GET("/metrics", gin.WrapH(promhttp.Handler()))
}

// healthzHandler liveness: only indicates the process's event loop can still
// respond; it does not depend on SQL / Redis.
func healthzHandler(c *gin.Context) { c.String(200, "ok") }

// readyzHandler readiness: checks each injected dependency (SQL / Redis) in
// turn, returning 503 on any failure — so k8s pulls traffic off this pod
// instead of routing requests into an instance that's bound to 503 (docs/06
// §13). Kafka / outbox are not checked: a failed usage publish should not
// pull traffic.
//
// Degrades to a static 200 when no checks are injected (test / unassembled
// scenarios).
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
