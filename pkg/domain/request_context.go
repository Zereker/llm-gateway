package domain

import (
	"context"
	"time"
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
	//
	// trace_id / span_id **不**作为 RC 字段——它们是 OTel SpanContext 的内容，
	// M1 已经把 SpanContext 注入 RC.Ctx；要拿 string 形态用 middleware.TraceIDFromCtx /
	// middleware.SpanIDFromCtx 提取。这避免双源（ctx 和 flat field）drift。
	//
	// RequestID 是网关内部 per-call audit ID（**不是** OTel 概念，所以保留为 RC 字段）。
	RequestID string // 形如 req_<12hex>；同请求唯一；客户端报障定位用
	StartTime time.Time

	// === M2 Auth 写入 ===
	Identity UserIdentity

	// === M3 Envelope 写入 ===
	Envelope *RequestEnvelope

	// M4 Budget 不写 RC——Gate.Check 失败直接 abort；通过则 c.Next()，下游不需要
	// 知道 BudgetStatus（"通过"是隐含条件）。BudgetStatus 类型仍在 domain 用于
	// BudgetGate 接口返回，不在 RC 上 cache。

	// === M5 ModelService 写入 ===
	ModelService *ModelServiceSnapshot
	Pricing      PricingSnapshot

	// === M6 Limit 写入 ===
	// RateLimit 携带：(a) M10 调账需要的 TPM bucket keys + 估值；(b) headers 需要的 tightest bucket 状态。
	// nil = 没有任何 quota policy 应用到本请求（pin/apikey 都没绑 policy）。
	RateLimit *RateLimitState

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
	//
	// 没有 *gin.Context——response 写出由 middleware（schedule.go 里的 c.Writer）
	// 直接用 gin handler 收到的 *gin.Context 完成；不经 RC。Adapter 已 slim 化为
	// 只构 HTTP request，不写 response。
	//
	// 没有 *slog.Logger——日志走 slog.InfoContext / ErrorContext 等带 ctx 的 API；
	// trace.CtxHandler 自动从 ctx 抽 trace_id / span_id / baggage 字段（user_id 等）
	// 加到 record。详见 pkg/trace/sloghandler.go。
	Ctx context.Context // 业务 context（带 OTel SpanContext + Baggage）；timeout / cancel / log 都走它

	// === 扩展点 ===
	Extras map[string]any // 临时性 / 实验性字段；正式字段必须升级到 struct
}
