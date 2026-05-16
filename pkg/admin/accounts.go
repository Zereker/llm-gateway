package admin

import (
	"github.com/gin-gonic/gin"
)

// registerAccountRoutes 注册 /admin/v1/accounts CRUD。
//
//	GET    /accounts                列表
//	POST   /accounts                创建（pin + name 必填）
//	GET    /accounts/:pin           详情
//	PUT    /accounts/:pin           改 name / enabled / quota_policy_id
//	DELETE /accounts/:pin           软删
func registerAccountRoutes(api *gin.RouterGroup, s *AccountStore) {
	api.GET("/accounts", listAccounts(s))
	api.POST("/accounts", createAccount(s))
	api.GET("/accounts/:pin", getAccount(s))
	api.PUT("/accounts/:pin", updateAccount(s))
	api.DELETE("/accounts/:pin", deleteAccount(s))
}

func listAccounts(s *AccountStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		all, err := s.List(c.Request.Context())
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		items := make([]accountDTO, len(all))
		for i := range all {
			items[i] = accountToDTO(&all[i])
		}
		c.JSON(200, gin.H{"items": items})
	}
}

func getAccount(s *AccountStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		t, err := s.GetByPin(c.Request.Context(), c.Param("pin"))
		if err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, accountToDTO(t))
	}
}

func createAccount(s *AccountStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		var dto accountDTO
		if err := c.ShouldBindJSON(&dto); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		t := dtoToAccount(dto)
		if err := s.Create(c.Request.Context(), t); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, accountToDTO(t))
	}
}

func updateAccount(s *AccountStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req accountUpdateRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		updates := AccountUpdates{
			Name:             req.Name,
			Enabled:          req.Enabled,
			QuotaPolicyID:    req.QuotaPolicyID,
			ClearQuotaPolicy: req.ClearQuotaPolicy,
		}
		pin := c.Param("pin")
		if err := s.Update(c.Request.Context(), pin, updates); err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		t, err := s.GetByPin(c.Request.Context(), pin)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, accountToDTO(t))
	}
}

func deleteAccount(s *AccountStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := s.Delete(c.Request.Context(), c.Param("pin")); err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.Status(204)
	}
}

// accountUpdateRequest PUT body；nil = 不动。
type accountUpdateRequest struct {
	Name             *string `json:"name,omitempty"`
	Enabled          *bool   `json:"enabled,omitempty"`
	QuotaPolicyID    *int64  `json:"quota_policy_id,omitempty"`
	ClearQuotaPolicy bool    `json:"clear_quota_policy,omitempty"`
}
