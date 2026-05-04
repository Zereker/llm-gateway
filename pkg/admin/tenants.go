package admin

import (
	"github.com/gin-gonic/gin"
)

// registerTenantRoutes 注册 /admin/v1/tenants CRUD。
//
//	GET    /tenants                列表
//	POST   /tenants                创建（pin + name 必填）
//	GET    /tenants/:pin           详情
//	PUT    /tenants/:pin           改 name / enabled / quota_policy_id
//	DELETE /tenants/:pin           软删
func registerTenantRoutes(api *gin.RouterGroup, s *TenantStore) {
	api.GET("/tenants", listTenants(s))
	api.POST("/tenants", createTenant(s))
	api.GET("/tenants/:pin", getTenant(s))
	api.PUT("/tenants/:pin", updateTenant(s))
	api.DELETE("/tenants/:pin", deleteTenant(s))
}

func listTenants(s *TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		all, err := s.List(c.Request.Context())
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		items := make([]tenantDTO, len(all))
		for i := range all {
			items[i] = tenantToDTO(&all[i])
		}
		c.JSON(200, gin.H{"items": items})
	}
}

func getTenant(s *TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		t, err := s.GetByPin(c.Request.Context(), c.Param("pin"))
		if err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, tenantToDTO(t))
	}
}

func createTenant(s *TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		var dto tenantDTO
		if err := c.ShouldBindJSON(&dto); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		t := dtoToTenant(dto)
		if err := s.Create(c.Request.Context(), t); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, tenantToDTO(t))
	}
}

func updateTenant(s *TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req tenantUpdateRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		updates := TenantUpdates{
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
		c.JSON(200, tenantToDTO(t))
	}
}

func deleteTenant(s *TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := s.Delete(c.Request.Context(), c.Param("pin")); err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.Status(204)
	}
}

// tenantUpdateRequest PUT body；nil = 不动。
type tenantUpdateRequest struct {
	Name             *string `json:"name,omitempty"`
	Enabled          *bool   `json:"enabled,omitempty"`
	QuotaPolicyID    *int64  `json:"quota_policy_id,omitempty"`
	ClearQuotaPolicy bool    `json:"clear_quota_policy,omitempty"`
}
