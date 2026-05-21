package domain

import (
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

	// === M3 Envelope 写入默认值；后续 middleware 可覆盖（多租户 / 灰度场景） ===
	//
	// 类型 = protocol.Lookup（用 any 是为了避 pkg/domain → pkg/dispatch →
	// pkg/protocol → pkg/adapter → pkg/domain 循环依赖）。访问走
	// middleware.HandlersFrom(rc) 类型安全 helper，不要直接 type-assert。
	//
	// **v0.6 融合**：原 v0.5 把 adapter / translator 两个独立 lookup 挂在 RC 上
	// （rc.Adapters + rc.Translators），消费侧两次查找；v0.6 融合成单一
	// protocol.Lookup，consumer 只需要 (endpoint, srcProto) → Handler 一次。
	//
	// 默认值 protocol.DefaultLookup 包装全局 adapter + translator registry。
	Handlers any

	// === M5 ModelService 写入（原始请求 model） ===
	//
	// **重要**：M5 不查 active pricing（docs/01 §7、docs/05 §6）。Pricing 匹配由
	// 下游计费平台按 Usage Event 的 RequestTime 完成；网关不维护 PricingSnapshot。
	ModelService *ModelService

	// === M5 ModelService 写入（预解析的尝试序列） ===
	//
	// ModelChain[0] = primary（== ModelService）；后续 = X-Gateway-Fallback-Models
	// 按声明顺序去重并经过 catalog + subscription 校验后保留的 fallback model。
	// 未声明 fallback 时长度 = 1。M7 outer loop 直接遍历这个序列，不再重做 M5。
	ModelChain []*ModelService

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

	// === 扩展点 ===
	//
	// **context.Context 不在 RC 里**：单源真相是 `c.Request.Context()`，每个 mw
	// 通过 `c.Request = c.Request.WithContext(ctx)` 接力。把 ctx 字段挂在 mutable
	// struct 上违反 Go 「context is values, not state」原则，还会跟 gin 原生
	// `c.Request.Context()` drift（M1 之后只更新 RC.Ctx 不更新 c.Request 时丢 span）。
	Extras map[string]any // 临时性 / 实验性字段
}
