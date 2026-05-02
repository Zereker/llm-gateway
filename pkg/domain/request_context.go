package domain

import (
	"context"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
)

// RequestContext 一次 HTTP 请求的全链路可变状态。
//
// 写入规则：
//   - 每个字段标注了写入它的 middleware（M1-M10）
//   - 后注册的 middleware 不应改写前者已写入的字段（除注释明确允许）
//   - Handler / Adapter 视为只读消费者；Usage / Error / SchedulingDecision 是响应阶段产物
//
// 读取规则：
//   - 通过 pkg/middleware.GetRequestContext(c) 取出，杜绝裸调 c.MustGet/c.Get
//
// 演进规则见 docs/architecture/01-request-pipeline.md 第 11 节。
type RequestContext struct {
	// === M1 TraceContext 写入 ===
	TraceID   string
	RequestID string
	StartTime time.Time

	// === M2 Auth 写入 ===
	Identity UserIdentity

	// === M3 Envelope 写入 ===
	Envelope *RequestEnvelope

	// === M4 Budget 写入 ===
	BudgetStatus BudgetStatus

	// === M5 ModelService 写入 ===
	ModelService *ModelServiceSnapshot
	Pricing      PricingSnapshot

	// === M6 Limit 写入 ===
	LimitSpec *LimitSpec

	// === M7 Schedule 写入 ===
	Endpoint *Endpoint
	// 注意：adapter.Session 不挂在 RC 上 —— 仅在 RetryExecutor 内活到 Finalize；
	// 输出（Usage / Error / SchedulingDecision）已通过下面的字段返还给 RC。
	// 需要在 trace / metric 中标注厂商时，从 Endpoint.Vendor + ModelService.Model 取。

	// === 响应阶段写入（M7 内部 / Adapter） ===
	Usage              *Usage
	Error              *AdapterError
	SchedulingDecision *SchedulingDecision

	// === 全链路共享（M1 写入，后续只读） ===
	Ctx    context.Context // 业务 context，用于 timeout / cancel
	GinCtx *gin.Context    // HTTP 写器（Adapter 流式写出时需要）
	Logger *slog.Logger    // 已带 trace_id / user_id 等基础字段

	// === 扩展点 ===
	Extras map[string]any // 临时性 / 实验性字段；正式字段必须升级到 struct
}
