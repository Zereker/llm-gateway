package admin

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/repo"
)

// registerAPIKeyRoutes 注册 /admin/v1/apikeys CRUD。
//
//	POST   /apikeys                  创建（响应一次返明文 api_key）
//	GET    /apikeys                  列表（?tenant_id=&user_id=&enabled= 过滤）
//	GET    /apikeys/:api_key_id      详情（不返明文）
//	PUT    /apikeys/:api_key_id      更新 enabled / expires_at / group / external_user
//	DELETE /apikeys/:api_key_id      硬删
//
// v0.1 单租户：所有请求未指定 tenant_id 时按 "default" 处理。
func registerAPIKeyRoutes(api *gin.RouterGroup, s *APIKeyStore) {
	api.GET("/apikeys", listAPIKeys(s))
	api.POST("/apikeys", createAPIKey(s))
	api.GET("/apikeys/:api_key_id", getAPIKey(s))
	api.PUT("/apikeys/:api_key_id", updateAPIKey(s))
	api.DELETE("/apikeys/:api_key_id", deleteAPIKey(s))
}

func listAPIKeys(s *APIKeyStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID := tenantOrDefault(c.Query("tenant_id"))
		userIDFilter := c.Query("user_id")
		var enabledFilter *bool
		if v := c.Query("enabled"); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				c.JSON(400, gin.H{"error": "enabled must be true|false"})
				return
			}
			enabledFilter = &b
		}

		all, err := s.List(c.Request.Context(), tenantID, userIDFilter, enabledFilter)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		items := make([]apiKeyDTO, len(all))
		for i := range all {
			items[i] = apiKeyToDTO(&all[i]) // 不返明文
		}
		c.JSON(200, gin.H{"items": items})
	}
}

func getAPIKey(s *APIKeyStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID := tenantOrDefault(c.Query("tenant_id"))
		k, err := s.GetByAPIKeyID(c.Request.Context(), tenantID, c.Param("api_key_id"))
		if err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, apiKeyToDTO(k))
	}
}

func createAPIKey(s *APIKeyStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req apiKeyCreateRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if req.UserID == "" {
			c.JSON(400, gin.H{"error": "user_id required"})
			return
		}
		k := &repo.APIKey{
			TenantID:     tenantOrDefault(req.TenantID),
			UserID:       req.UserID,
			Group:        defaultIfEmpty(req.Group, "default"),
			ExternalUser: req.ExternalUser,
			ExpiresAt:    req.ExpiresAt,
		}
		if err := s.Create(c.Request.Context(), k); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		// 一次性返回明文 api_key（创建后 GET 不会再返）
		c.JSON(201, apiKeyCreateResponse{
			apiKeyDTO: apiKeyToDTO(k),
			APIKey:    k.Key,
		})
	}
}

func updateAPIKey(s *APIKeyStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req apiKeyUpdateRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		updates := APIKeyUpdates{
			Enabled:        req.Enabled,
			ExpiresAt:      req.ExpiresAt,
			ClearExpiresAt: req.ClearExpiresAt,
			Group:          req.Group,
			ExternalUser:   req.ExternalUser,
		}
		tenantID := tenantOrDefault(c.Query("tenant_id"))
		if err := s.Update(c.Request.Context(), tenantID, c.Param("api_key_id"), updates); err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		k, err := s.GetByAPIKeyID(c.Request.Context(), tenantID, c.Param("api_key_id"))
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, apiKeyToDTO(k))
	}
}

func deleteAPIKey(s *APIKeyStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID := tenantOrDefault(c.Query("tenant_id"))
		if err := s.Delete(c.Request.Context(), tenantID, c.Param("api_key_id")); err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.Status(204)
	}
}

func tenantOrDefault(t string) string {
	if t == "" {
		return "default"
	}
	return t
}

func defaultIfEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// apiKeyCreateRequest POST /apikeys 请求体。
type apiKeyCreateRequest struct {
	TenantID     string     `json:"tenant_id,omitempty"` // 不传则 "default"
	UserID       string     `json:"user_id"`
	Group        string     `json:"group,omitempty"`        // 不传则 "default"
	ExternalUser bool       `json:"external_user,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`   // 不传则永不过期
}

// apiKeyUpdateRequest PUT /apikeys/:api_key_id 请求体；nil = 不动。
type apiKeyUpdateRequest struct {
	Enabled        *bool      `json:"enabled,omitempty"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	ClearExpiresAt bool       `json:"clear_expires_at,omitempty"`
	Group          *string    `json:"group,omitempty"`
	ExternalUser   *bool      `json:"external_user,omitempty"`
}
