package console

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// NewEngine 装配控制面 gin.Engine：ops 路由（/healthz）公开，/admin/* 全部走
// adminAuth（认证 + 解析角色）。写路由（POST/DELETE）额外挂 requireAdmin——viewer
// 角色只能读。所有业务 handler 只依赖 *Store。
func NewEngine(store *Store, tokens []Token) *gin.Engine {
	engine := gin.New()
	engine.Use(gin.Recovery())

	engine.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })

	// Web UI（Phase 3）：单文件 admin 控制台。页面本身不含机密，鉴权发生在它发起的
	// /admin/* API 调用上（浏览器带 admin token）。
	engine.GET("/", func(c *gin.Context) { c.Data(http.StatusOK, "text/html; charset=utf-8", indexHTML) })

	api := &api{store: store}
	admin := engine.Group("/admin", adminAuth(tokens))
	{
		// 读：admin + viewer 都行
		admin.GET("/accounts", api.listAccounts)
		admin.GET("/model-services", api.listModelServices)
		admin.GET("/endpoints", api.listEndpoints)
		admin.GET("/endpoints/:id", api.getEndpoint)
		admin.GET("/accounts/:pin/api-keys", api.listAPIKeys)

		// 写：只有 admin
		admin.POST("/accounts", requireAdmin, api.createAccount)
		admin.POST("/model-services", requireAdmin, api.createModelService)
		admin.POST("/subscriptions", requireAdmin, api.subscribe)
		admin.POST("/endpoints", requireAdmin, api.createEndpoint)
		admin.DELETE("/endpoints/:id", requireAdmin, api.deleteEndpoint)
		admin.POST("/api-keys", requireAdmin, api.createAPIKey)
		admin.DELETE("/accounts/:pin/api-keys/:keyID", requireAdmin, api.revokeAPIKey)
	}
	return engine
}

// 注：用量/计量刻意不在控制面里聚合——网关只负责把 usage 事件经 outbox
// （file source-of-truth + Kafka 广播）产出，下游 metering/billing 系统消费。
// 控制面做成 usage 聚合会把"计费"这个独立复杂域拉进来，边界就破了。

type api struct {
	store *Store
}

// =============================================================================
// Accounts
// =============================================================================

func (a *api) createAccount(c *gin.Context) {
	var in AccountInput
	if !bind(c, &in) {
		return
	}
	if in.Pin == "" || in.Name == "" {
		abortError(c, 400, "invalid_argument", "pin and name are required")
		return
	}
	if err := a.store.CreateAccount(c.Request.Context(), in); err != nil {
		writeStoreErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"pin": in.Pin})
}

func (a *api) listAccounts(c *gin.Context) {
	rows, err := a.store.ListAccounts(c.Request.Context())
	if err != nil {
		writeStoreErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"accounts": rows})
}

// =============================================================================
// Model services + subscriptions
// =============================================================================

func (a *api) createModelService(c *gin.Context) {
	var in ModelServiceInput
	if !bind(c, &in) {
		return
	}
	if in.ServiceID == "" || in.Model == "" {
		abortError(c, 400, "invalid_argument", "service_id and model are required")
		return
	}
	id, err := a.store.CreateModelService(c.Request.Context(), in)
	if err != nil {
		writeStoreErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": id})
}

func (a *api) listModelServices(c *gin.Context) {
	rows, err := a.store.ListModelServices(c.Request.Context())
	if err != nil {
		writeStoreErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"model_services": rows})
}

func (a *api) subscribe(c *gin.Context) {
	var in SubscriptionInput
	if !bind(c, &in) {
		return
	}
	if in.AccountID == "" || in.ModelServiceID == 0 {
		abortError(c, 400, "invalid_argument", "account_id and model_service_id are required")
		return
	}
	if err := a.store.Subscribe(c.Request.Context(), in); err != nil {
		writeStoreErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"status": "subscribed"})
}

// =============================================================================
// Endpoints
// =============================================================================

func (a *api) createEndpoint(c *gin.Context) {
	var in EndpointInput
	if !bind(c, &in) {
		return
	}
	id, err := a.store.CreateEndpoint(c.Request.Context(), in)
	if err != nil {
		var invalid *InvalidEndpointError
		if errors.As(err, &invalid) {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": gin.H{"code": "endpoint_invalid", "message": "endpoint failed validation", "reasons": invalid.Reasons},
			})
			return
		}
		writeStoreErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": id})
}

func (a *api) listEndpoints(c *gin.Context) {
	rows, err := a.store.ListEndpoints(c.Request.Context())
	if err != nil {
		writeStoreErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"endpoints": rows})
}

func (a *api) getEndpoint(c *gin.Context) {
	id, ok := pathInt64(c, "id")
	if !ok {
		return
	}
	v, err := a.store.GetEndpoint(c.Request.Context(), id)
	if err != nil {
		writeStoreErr(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}

func (a *api) deleteEndpoint(c *gin.Context) {
	id, ok := pathInt64(c, "id")
	if !ok {
		return
	}
	if err := a.store.DeleteEndpoint(c.Request.Context(), id); err != nil {
		writeStoreErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

// =============================================================================
// API keys
// =============================================================================

func (a *api) createAPIKey(c *gin.Context) {
	var in APIKeyInput
	if !bind(c, &in) {
		return
	}
	if in.SubAccountID == "" {
		abortError(c, 400, "invalid_argument", "sub_account_id is required")
		return
	}
	created, err := a.store.CreateAPIKey(c.Request.Context(), in)
	if err != nil {
		writeStoreErr(c, err)
		return
	}
	// 明文只此一次返回。
	c.JSON(http.StatusCreated, created)
}

func (a *api) listAPIKeys(c *gin.Context) {
	rows, err := a.store.ListAPIKeys(c.Request.Context(), c.Param("pin"))
	if err != nil {
		writeStoreErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"api_keys": rows})
}

func (a *api) revokeAPIKey(c *gin.Context) {
	if err := a.store.RevokeAPIKey(c.Request.Context(), c.Param("pin"), c.Param("keyID")); err != nil {
		writeStoreErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "revoked"})
}

// =============================================================================
// helpers
// =============================================================================

// bind 解析 JSON body；失败时已写 400，返回 false。
func bind(c *gin.Context, dst any) bool {
	if err := c.ShouldBindJSON(dst); err != nil {
		abortError(c, 400, "invalid_json", err.Error())
		return false
	}
	return true
}

func pathInt64(c *gin.Context, name string) (int64, bool) {
	v, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil {
		abortError(c, 400, "invalid_argument", name+" must be an integer")
		return 0, false
	}
	return v, true
}

// writeStoreErr 把 store 错误翻成 HTTP：NotFound→404，唯一键冲突→409，其余→500。
// 内部错误细节只进日志层（gin.Recovery / slog），不回客户端 body。
func writeStoreErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		abortError(c, 404, "not_found", "resource not found")
	case isDuplicateKey(err):
		abortError(c, 409, "conflict", "resource already exists (unique key violation)")
	default:
		abortError(c, 500, "internal", "internal error")
	}
}

// isDuplicateKey 识别 MySQL 1062 唯一键冲突（不依赖 driver 具体类型，匹配错误串）。
func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return containsAny(s, "Duplicate entry", "Error 1062")
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// abortError 统一错误响应体 {"error":{"code","message"}}。
func abortError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}
