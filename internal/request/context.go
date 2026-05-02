// Package request 定义跨 middleware 的请求级状态 Context，并封装 gin.Context 存取。
//
// 所有跨 middleware 状态都装进 typed *Context 一个 struct（保证类型安全），
// 通过 gin.Context.Set/Get（私有 key）传递（保证与 gin 生态兼容、不污染函数签名）。
//
// 详见 docs/architecture/01-request-pipeline.md。
package request

import (
	"context"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/internal/adapter"
	"github.com/zereker-labs/ai-gateway/internal/budget"
	"github.com/zereker-labs/ai-gateway/internal/envelope"
	"github.com/zereker-labs/ai-gateway/internal/errs"
	"github.com/zereker-labs/ai-gateway/internal/identity"
	"github.com/zereker-labs/ai-gateway/internal/limit"
	"github.com/zereker-labs/ai-gateway/internal/modelservice"
	"github.com/zereker-labs/ai-gateway/internal/scheduling"
	"github.com/zereker-labs/ai-gateway/internal/usage"
)

// Context 一次 HTTP 请求的全链路可变状态。
//
// 写入规则：
//   - 每个字段标注了写入它的 middleware（M1-M10）
//   - 后注册的 middleware 不应改写前者已写入的字段（除注释明确允许）
//   - Handler / Adapter 视为只读消费者；Usage / Error / SchedulingDecision 是响应阶段产物
//
// 读取规则：
//   - 通过 request.From(c) 取出，杜绝裸调 c.MustGet/c.Get
//
// 演进规则见 docs/architecture/01 第 11 节。
type Context struct {
	// === M1 TraceContext 写入 ===
	TraceID   string
	RequestID string
	StartTime time.Time

	// === M2 Auth 写入 ===
	Identity identity.User

	// === M3 Envelope 写入 ===
	Envelope *envelope.Envelope

	// === M4 Budget 写入 ===
	BudgetStatus budget.Status

	// === M5 ModelService 写入 ===
	ModelService *modelservice.Snapshot
	Pricing      modelservice.PricingSnapshot

	// === M6 Limit 写入 ===
	LimitSpec *limit.Spec

	// === M7 Schedule 写入 ===
	Endpoint *scheduling.Endpoint
	Adapter  adapter.Adapter

	// === 响应阶段写入（M7 内部 / Adapter） ===
	Usage              *usage.Usage
	Error              *errs.Error
	SchedulingDecision *scheduling.Decision

	// === 全链路共享（M1 写入，后续只读） ===
	Ctx    context.Context // 业务 context，用于 timeout / cancel
	GinCtx *gin.Context    // HTTP 写器（Adapter 流式写出时需要）
	Logger *slog.Logger    // 已带 trace_id / user_id 等基础字段

	// === 扩展点 ===
	Extras map[string]any // 临时性 / 实验性字段；正式字段必须升级到 struct
}
