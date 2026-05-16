package domain

import (
	"context"
	"time"
)

// RequestContext 一次 HTTP 请求的全链路可变状态。
//
// 字段定义参见 docs/architecture/01-request-pipeline.md §3。
//
// 写入规则：
//   - 每个字段标注了写入它的 middleware（M1-M10）
//   - 后注册的 middleware 不应改写前者已写入的字段（除注释明确允许）
//   - Handler / Adapter 视为只读消费者；Usage / Error / SchedulingDecision 是响应阶段产物
//
// 读取规则：
//   - 通过 pkg/middleware.GetRequestContext(c) 取出，杜绝裸调 c.MustGet/c.Get
type RequestContext struct {
	// === M1 TraceContext 写入 ===
	//
	// trace_id / span_id **不**作为 RC 字段——它们是 OTel SpanContext 的内容；
	// 要拿 string 形态用 middleware.TraceIDFromCtx 提取。这避免双源 drift。
	RequestID string    // 形如 req_<12hex>；同请求唯一；客户端报障定位用
	StartTime time.Time

	// === M2 Auth 写入 ===
	Identity UserIdentity

	// === M3 Envelope 写入 ===
	Envelope *RequestEnvelope

	// === M5 ModelService 写入（原始请求 model） ===
	//
	// **重要**：M5 不查 active pricing（docs/01 §7、docs/05 §6）。Pricing 匹配由
	// 下游计费平台按 Usage Event 的 RequestTime 完成；网关不维护 PricingSnapshot。
	ModelService *ModelService

	// === M7 写入（实际成功 model） ===
	//
	// 跨 model fallback 时 RoutedModelService != ModelService。
	// 没 fallback 时 RoutedModelService == ModelService。
	RoutedModelService *ModelService

	// === M6 Limit 写入 ===
	// nil = 没有任何 quota policy 应用到本请求。
	RateLimit *RateLimitState

	// === M7 Schedule 写入 ===
	Endpoint *Endpoint

	// === 响应阶段写入（M7 内部 / M10） ===
	Usage              *Usage
	Error              *AdapterError
	SchedulingDecision *SchedulingDecision

	// === 全链路共享 ===
	Ctx context.Context // 业务 context（带 OTel SpanContext + Baggage）

	// === 扩展点 ===
	Extras map[string]any // 临时性 / 实验性字段
}
