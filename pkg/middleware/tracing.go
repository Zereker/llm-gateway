package middleware

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/metric"
	"github.com/zereker-labs/ai-gateway/pkg/trace"
	"github.com/zereker-labs/ai-gateway/pkg/usage"
)

// TracingDeps M10 Tracing middleware 的依赖。
type TracingDeps struct {
	Outbox usage.OutboxPublisher
	Tracer trace.Tracer
}

// Tracing 是 M10：聚合 metric + 发计量事件 + 写 SchedulingDecision trace。
// 在 c.Next() 之后执行（defer 模式）。
//
// 发布失败不影响业务返回（best-effort）。
//
// 用 context.Background()（带 5s 超时）发 Outbox，避免 client 已断开时
// 还是要把 usage 落出（计费需要）。
func Tracing(deps TracingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()

		rc := TryGetRequestContext(c)
		if rc == nil {
			return
		}

		cost := time.Since(rc.StartTime).Milliseconds()
		metric.Observe(metric.HTTPRequestDurationMs, float64(cost),
			"method", c.Request.Method,
			"path", c.FullPath(),
			"status", strconv.Itoa(c.Writer.Status()))

		if rc.Usage != nil && deps.Outbox != nil {
			publishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			payload, err := json.Marshal(rc.Usage)
			if err == nil {
				key := ""
				if rc.Endpoint != nil {
					key = rc.Endpoint.ID
				}
				_ = deps.Outbox.Publish(publishCtx, &usage.OutboxEvent{
					Payload: payload,
					Key:     key,
				})
			}
		}

		if rc.SchedulingDecision != nil && deps.Tracer != nil {
			deps.Tracer.Log(rc.Ctx, "scheduling_decision", rc.SchedulingDecision)
		}
	}
}
