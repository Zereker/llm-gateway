// Package admin 装配 admin（控制平面）的 gin.Engine：
// X-Admin-Token 鉴权 + /admin/v1 下的各类 CRUD。
//
// 与 pkg/router 平行：cmd/admin/main.go 是薄 lifecycle 壳，所有业务逻辑都在本包内。
//
// 数据层用 gorm（不是 pkg/repo 的 sqlx）——admin 业务繁琐（CRUD / 过滤 / 分页 /
// 未来 soft-delete / audit），gorm 减少样板。pkg/repo 仍是 gateway 的读路径，
// 两套库各自服务于"谁的 hot path"。
//
// 实体类型住在 pkg/repo——admin 和 gateway 共享一份 struct 定义
// （带 sqlx db: + gorm: 双标签 + 自定义 Scanner/Valuer）。
//
// schema 真相：pkg/infra/schema.sql。admin 启动时 infra.Migrate 跑一次 raw SQL；
// gorm tag 只描述列名 / 类型，**不开 AutoMigrate**——schema 演进只能从 SQL 走。
package admin

import (
	"github.com/gin-gonic/gin"
)

// Deps 是 NewEngine 的依赖集合。
//
// **v0.3 新增**：AccountStore / QuotaPolicyStore / SubscriptionStore
//
// 不抽 interface（无第二实现 + admin tests 直接用真 MySQL）。
type Deps struct {
	Token             string
	AccountStore      *AccountStore
	QuotaPolicyStore  *QuotaPolicyStore
	ModelServiceStore *ModelServiceStore
	SubscriptionStore *SubscriptionStore
	EndpointStore     *EndpointStore
	APIKeyStore       *APIKeyStore
	PricingStore      *PricingStore
}

// NewEngine 构造 admin gin.Engine 并完成全部装配。
//
// 路由：
//   - GET /healthz, /readyz                 ops 探活，不走 admin token
//   - /admin/v1/accounts*                    Account CRUD（要 token）
//   - /admin/v1/accounts/:pin/subscriptions* account 订阅模型 CRUD
//   - /admin/v1/quota-policies*             QuotaPolicy CRUD
//   - /admin/v1/modelservices*              ModelService CRUD（全局 catalog）
//   - /admin/v1/endpoints*                  Endpoint CRUD（全局上游池）
//   - /admin/v1/apikeys*                    APIKey CRUD
//   - /admin/v1/modelservices/:model/prices* Pricing rotate / list
func NewEngine(deps Deps) *gin.Engine {
	if gin.Mode() == gin.DebugMode {
		gin.SetMode(gin.ReleaseMode)
	}
	engine := gin.New()
	engine.Use(gin.Recovery())

	registerOpsRoutes(engine)

	api := engine.Group("/admin/v1", authMW(deps.Token))
	registerAccountRoutes(api, deps.AccountStore)
	registerSubscriptionRoutes(api, deps.SubscriptionStore)
	registerQuotaPolicyRoutes(api, deps.QuotaPolicyStore)
	registerModelServiceRoutes(api, deps.ModelServiceStore)
	registerEndpointRoutes(api, deps.EndpointStore)
	registerAPIKeyRoutes(api, deps.APIKeyStore)
	registerPricingRoutes(api, deps.ModelServiceStore, deps.PricingStore)

	return engine
}

func registerOpsRoutes(engine *gin.Engine) {
	engine.GET("/healthz", func(c *gin.Context) { c.String(200, "ok") })
	engine.GET("/readyz", func(c *gin.Context) { c.String(200, "ok") })
}
