package admin

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/repo"
)

// registerAPIKeyRoutes 注册 /admin/v1/apikeys CRUD。
//
//	POST   /apikeys                        创建（响应一次返明文 api_key）
//	GET    /apikeys                        列表（?account_id=&sub_account_id=&enabled= 过滤）
//	GET    /apikeys/:api_key_id            详情（不返明文，返 prefix）
//	PUT    /apikeys/:api_key_id            更新 enabled / expires_at / group / external_user / name
//	POST   /apikeys/:api_key_id/revoke     主动吊销（set revoked_at）
//	DELETE /apikeys/:api_key_id            软删
//
// v0.1 单主账号：所有请求未指定 account_id 时按 "default" 处理。
func registerAPIKeyRoutes(api *gin.RouterGroup, s *APIKeyStore) {
	api.GET("/apikeys", listAPIKeys(s))
	api.POST("/apikeys", createAPIKey(s))
	api.GET("/apikeys/:api_key_id", getAPIKey(s))
	api.PUT("/apikeys/:api_key_id", updateAPIKey(s))
	api.POST("/apikeys/:api_key_id/revoke", revokeAPIKey(s))
	api.DELETE("/apikeys/:api_key_id", deleteAPIKey(s))
}

func listAPIKeys(s *APIKeyStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		accountID := accountOrDefault(c.Query("account_id"))
		subAccountIDFilter := c.Query("sub_account_id")
		var enabledFilter *bool
		if v := c.Query("enabled"); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				c.JSON(400, gin.H{"error": "enabled must be true|false"})
				return
			}
			enabledFilter = &b
		}

		all, err := s.List(c.Request.Context(), accountID, subAccountIDFilter, enabledFilter)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		items := make([]apiKeyDTO, len(all))
		for i := range all {
			items[i] = apiKeyToDTO(&all[i])
		}
		c.JSON(200, gin.H{"items": items})
	}
}

func getAPIKey(s *APIKeyStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		accountID := accountOrDefault(c.Query("account_id"))
		k, err := s.GetByAPIKeyID(c.Request.Context(), accountID, c.Param("api_key_id"))
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
		if req.SubAccountID == "" {
			c.JSON(400, gin.H{"error": "sub_account_id required"})
			return
		}
		k := &repo.APIKey{
			AccountID:     accountOrDefault(req.AccountID),
			Name:          req.Name,
			SubAccountID:  req.SubAccountID,
			Group:         defaultIfEmpty(req.Group, "default"),
			ExternalUser:  req.ExternalUser,
			ExpiresAt:     req.ExpiresAt,
			QuotaPolicyID: req.QuotaPolicyID,
		}
		plaintext, err := s.Create(c.Request.Context(), k)
		if err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		// 一次性返回明文 api_key（创建后 GET 只能拿到 prefix）
		c.JSON(201, apiKeyCreateResponse{
			apiKeyDTO: apiKeyToDTO(k),
			APIKey:    plaintext,
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
			Enabled:          req.Enabled,
			ExpiresAt:        req.ExpiresAt,
			ClearExpiresAt:   req.ClearExpiresAt,
			Group:            req.Group,
			ExternalUser:     req.ExternalUser,
			Name:             req.Name,
			QuotaPolicyID:    req.QuotaPolicyID,
			ClearQuotaPolicy: req.ClearQuotaPolicy,
		}
		accountID := accountOrDefault(c.Query("account_id"))
		if err := s.Update(c.Request.Context(), accountID, c.Param("api_key_id"), updates); err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		k, err := s.GetByAPIKeyID(c.Request.Context(), accountID, c.Param("api_key_id"))
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, apiKeyToDTO(k))
	}
}

func revokeAPIKey(s *APIKeyStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		accountID := accountOrDefault(c.Query("account_id"))
		if err := s.Revoke(c.Request.Context(), accountID, c.Param("api_key_id")); err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		k, err := s.GetByAPIKeyID(c.Request.Context(), accountID, c.Param("api_key_id"))
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, apiKeyToDTO(k))
	}
}

func deleteAPIKey(s *APIKeyStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		accountID := accountOrDefault(c.Query("account_id"))
		if err := s.Delete(c.Request.Context(), accountID, c.Param("api_key_id")); err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.Status(204)
	}
}

func accountOrDefault(t string) string {
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
	AccountID     string     `json:"account_id,omitempty"` // 不传则 "default"
	SubAccountID  string     `json:"sub_account_id"`
	Name          string     `json:"name,omitempty"`  // 用户友好标签：prod/dev/ci-bot
	Group         string     `json:"group,omitempty"` // 不传则 "default"
	ExternalUser  bool       `json:"external_user,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`      // 不传则永不过期
	QuotaPolicyID *int64     `json:"quota_policy_id,omitempty"` // 可选；不传 = key 维度不限
}

// apiKeyUpdateRequest PUT /apikeys/:api_key_id 请求体；nil = 不动。
type apiKeyUpdateRequest struct {
	Enabled          *bool      `json:"enabled,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	ClearExpiresAt   bool       `json:"clear_expires_at,omitempty"`
	Group            *string    `json:"group,omitempty"`
	ExternalUser     *bool      `json:"external_user,omitempty"`
	Name             *string    `json:"name,omitempty"`
	QuotaPolicyID    *int64     `json:"quota_policy_id,omitempty"`
	ClearQuotaPolicy bool       `json:"clear_quota_policy,omitempty"`
}
