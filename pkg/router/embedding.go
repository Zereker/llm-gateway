package router

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/middleware"
)

// registerEmbeddingRoutes 注册 embedding 模态路由 + 它专属的 middleware 链。
//
// 路径（每条 `.POST` 自带 /v1 完整前缀）：
//
//	POST /v1/embeddings  OpenAI / OpenAI-compat
//
// OpenAI Adapter 的 Metadata.SupportedModalities 已含 ModalityEmbedding，
// 配一个 embedding model + endpoint 就能用。
func registerEmbeddingRoutes(engine *gin.Engine, deps Deps) {
	pre := engine.Group("/",
		middleware.BodyLimit(deps.BodyLimit),
		middleware.Timeout(deps.Timeout),
		middleware.TraceContext(),
		// M10 Tracing 挂在 Recover **外层**（post-c.Next() 收尾）：
		//   - 任何后续 middleware abort（401/429/503）→ 洋葱返程仍执行收尾
		//     ——请求 metric / usage 事件 / decision 审计没有盲区
		//   - panic → 内层 Recover 先恢复并写 500，控制流正常返回，收尾照跑
		//     且看到的是最终 500 状态
		// （旧版挂链尾，abort 一律跳过 → 429 风暴在 M10 指标里隐身）
		middleware.Tracing(
			middleware.WithUsageOutbox(deps.UsageOutbox),
			middleware.WithTracer(deps.AuditTracer),
		),
		middleware.Recover(),
		middleware.Auth(middleware.WithIdentityProvider(deps.IdentityProvider)),
	)
	pre.POST("/v1/embeddings",
		middleware.WithSourceProtocol(domain.ProtoOpenAI, domain.ModalityEmbedding),
		middleware.Envelope(),
		middleware.Budget(middleware.WithBudgetGate(deps.BudgetGate)),
		middleware.ModelService(
			middleware.WithModelCatalog(deps.ModelCatalog),
			middleware.WithSubscriptionChecker(deps.SubscriptionChecker),
		),
		middleware.Moderation(middleware.WithModerator(deps.Moderator)),
		middleware.Limit(
			middleware.WithLimitStore(deps.RateLimitStore),
			middleware.WithLimitPolicies(deps.QuotaPolicies),
		),
		// embeddings 天然确定（无采样参数）——ResponseCache 默认就缓存，命中直接返
		// 向量、跳过上游。见 middleware.ResponseCache 的 embeddings 例外。
		//
		// **专用 EmbeddingCache（精确），不复用 deps.Cache**：当全局配的是语义缓存时
		// deps.Cache 是 SemanticCache，而 embedding 请求的 input 会被 extractPrompt 抽
		// 去做相似度匹配——语义命中对 embedding 是错的（paraphrase 的正确向量不同）。
		// embeddings 必须精确缓存。
		deps.EmbeddingCache,
		middleware.Schedule(deps.Dispatcher),
		noopHandler,
	)
}
