package middleware

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/metric"
	"github.com/zereker-labs/ai-gateway/pkg/ratelimit"
	"github.com/zereker-labs/ai-gateway/pkg/trace"
	"github.com/zereker-labs/ai-gateway/pkg/usage"
)

// TracingDeps M10 Tracing middleware 的依赖。
//
// **RateLimitStore**：M6 reserve 时按 estimated tokens 扣 TPM 桶；M10 拿到真实
// usage 后调账（AdjustBatch）让下个请求看到准确剩余配额。
type TracingDeps struct {
	Outbox          usage.OutboxPublisher
	Tracer          trace.Tracer
	RateLimitStore  ratelimit.Store
}

// Tracing 是 M10：聚合 metric + 发计量事件 + 写 SchedulingDecision trace。
// 在 c.Next() 之后执行（defer 模式）。
//
// 发布失败不影响业务返回（best-effort）。
//
// 用 context.Background()（带 5s 超时）发 Outbox，避免 client 已断开时
// 还是要把 usage 落出（计费需要）。
//
// **填 Meta**：dump 前从 rc 各字段聚合 UsageMeta —— Pricing 是 billing 的灵魂指针，
// 缺了 billing pipeline 完全没法工作；其它 ID/时间维度也都填上方便审计。
func Tracing(deps TracingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()

		rc := TryGetRequestContext(c)
		if rc == nil {
			return
		}

		now := time.Now().UTC()
		cost := now.Sub(rc.StartTime).Milliseconds()
		metric.Observe(metric.HTTPRequestDurationMs, float64(cost),
			"method", c.Request.Method,
			"path", c.FullPath(),
			"status", strconv.Itoa(c.Writer.Status()))

		if rc.Usage != nil {
			fillUsageMeta(rc, now, cost)
			// TPM commit：reserve 估值 vs 实际差额调账（best-effort，失败仅 log）
			adjustTPM(rc, deps.RateLimitStore)
		}

		if rc.Usage != nil && deps.Outbox != nil {
			publishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			payload, err := json.Marshal(rc.Usage)
			if err == nil {
				key := ""
				if rc.Endpoint != nil {
					key = strconv.FormatInt(rc.Endpoint.ID, 10)
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

// adjustTPM 根据 rc.Usage.Total - rc.RateLimit.ReservedTPM 调账 M6 预扣的 TPM 桶。
//
// **best-effort**：失败仅 log，不影响响应（请求已成功；调账失败下个请求看到的剩余配额
// 偏不准，acceptable）。
//
// **窗口写死 1min**：M6 buildBuckets 里 TPM bucket Window 也是 time.Minute；
// 两边必须一致。如果将来 TPM 改 5min 等，要把 window 也存进 RateLimitState。
func adjustTPM(rc *domain.RequestContext, store ratelimit.Store) {
	if store == nil || rc.RateLimit == nil || len(rc.RateLimit.TPMBucketKeys) == 0 {
		return
	}
	delta := int32(rc.Usage.Total) - int32(rc.RateLimit.ReservedTPM)
	if delta == 0 {
		return
	}
	adjustments := make([]ratelimit.BucketAdjust, len(rc.RateLimit.TPMBucketKeys))
	for i, k := range rc.RateLimit.TPMBucketKeys {
		adjustments[i] = ratelimit.BucketAdjust{
			Key:    k,
			Delta:  delta,
			Window: time.Minute,
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = store.AdjustBatch(ctx, adjustments) // log 等下版加；现在静默
}

// fillUsageMeta 把 rc 全链路状态聚合到 rc.Usage.Meta，给下游 billing 用。
//
// 各字段来源：
//   - ID 类（Model/Vendor/EndpointID/Service/User/APIKey/Trace/Request）：M2/M5/M7 沉淀的状态
//   - 时间类（Start/End/TotalLatency）：M1 的 StartTime + 现在
//   - **Pricing**：M5 拍的快照（billing pipeline 据此 join pricing_versions 拿真实价格）
//
// 防御性：rc.Endpoint / rc.ModelService 可能为 nil（M5/M7 未跑通就走到了 M10 的失败路径），
// 此时对应字段留空，不要 nil deref。
func fillUsageMeta(rc *domain.RequestContext, endTime time.Time, totalLatencyMs int64) {
	m := &rc.Usage.Meta

	// 时间维度
	m.StartTime = rc.StartTime
	m.EndTime = endTime
	m.TotalLatency = totalLatencyMs
	// TTFTMs 暂未捕获（要在 adapter session 第一个 Feed 时记一下时间，下个迭代再加）

	// 请求维度
	m.RequestID = rc.RequestID
	m.TraceID = rc.TraceID

	// 身份维度
	m.UserID = rc.Identity.UserID
	m.APIKeyID = rc.Identity.APIKeyID

	// 模型维度
	if rc.ModelService != nil {
		m.Model = rc.ModelService.Model
		m.ServiceID = rc.ModelService.ServiceID
	}

	// 路由维度
	if rc.Endpoint != nil {
		m.Vendor = rc.Endpoint.Vendor
		m.EndpointID = strconv.FormatInt(rc.Endpoint.ID, 10)
	}

	// 价格快照——billing pipeline 据此查 rule_json 算钱
	m.Pricing = rc.Pricing
}
