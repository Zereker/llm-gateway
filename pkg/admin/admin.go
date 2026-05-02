// Package admin 装配 admin（控制平面）的 gin.Engine：
// X-Admin-Token 鉴权 + /admin/v1 下的 ModelService / Endpoint CRUD。
//
// 与 pkg/router 平行：cmd/admin/main.go 是薄 lifecycle 壳，所有业务逻辑都在本包内。
// 与 pkg/router 的边界：本包专属于"控制平面 admin 服务"——独立 binary、独立鉴权方式、
// 独立路由前缀；不被 gateway 复用。
//
// Deps 用接口注入：本包只依赖 repo.{ModelService,Endpoint}Repository，
// 不知道 *sqlx.DB 也不打开连接（连接生命周期归 cmd 装配方）。
package admin

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/repo"
)

// Deps 是 NewEngine 的依赖集合。
//
// Token 校验 X-Admin-Token header；空时 admin 拒所有请求（防止误把无鉴权服务上线）。
// 两个 Repository 必须同时支持读和写——admin 同时承担列表查看 + 创建 / 修改 / 删除。
type Deps struct {
	Token            string
	ModelServiceRepo repo.ModelServiceRepository
	EndpointRepo     repo.EndpointRepository
}

// NewEngine 构造 admin gin.Engine 并完成全部装配。
//
// 路由：
//   - GET /healthz, /readyz                 ops 探活，不走 admin token
//   - /admin/v1/modelservices*              ModelService CRUD（要 token）
//   - /admin/v1/endpoints*                  Endpoint CRUD（要 token）
func NewEngine(deps Deps) *gin.Engine {
	if gin.Mode() == gin.DebugMode {
		gin.SetMode(gin.ReleaseMode)
	}
	engine := gin.New()
	engine.Use(gin.Recovery())

	registerOpsRoutes(engine)

	api := engine.Group("/admin/v1", authMW(deps.Token))
	registerModelServiceRoutes(api, deps.ModelServiceRepo)
	registerEndpointRoutes(api, deps.EndpointRepo)

	return engine
}

func registerOpsRoutes(engine *gin.Engine) {
	engine.GET("/healthz", func(c *gin.Context) { c.String(200, "ok") })
	engine.GET("/readyz", func(c *gin.Context) { c.String(200, "ok") })
}
