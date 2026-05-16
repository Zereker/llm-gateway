// Package router 装配 gin.Engine：注册按模态拆分的 LLM 路由 + 操作端点。
package router

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/middleware"
)

// Deps 是 NewEngine 的依赖集合：每个 middleware 的 options 列表。
//
// **Option pattern**：每个 middleware 接受自己的 `...XxxOption`，由调用方按需装配。
// 加新依赖只需 append 一个 With*，不动 Deps 结构。
//
// BodyLimit / Timeout 是 pre-middleware 标量参数。
type Deps struct {
	BodyLimit int64
	Timeout   time.Duration

	Auth         []middleware.AuthOption         // M2
	Budget       []middleware.BudgetOption       // M4
	ModelService []middleware.ModelServiceOption // M5
	Moderation   []middleware.ModerationOption   // M8
	Limit        []middleware.LimitOption        // M6
	Schedule     []middleware.ScheduleOption     // M7
	Tracing      []middleware.TracingOption      // M10
}

// NewEngine 构造 gin.Engine 并完成全部装配。
func NewEngine(deps Deps) *gin.Engine {
	engine := gin.New()

	registerOpsRoutes(engine)
	registerChatRoutes(engine, deps)
	registerImageRoutes(engine, deps)
	registerAudioRoutes(engine, deps)
	registerEmbeddingRoutes(engine, deps)

	return engine
}
