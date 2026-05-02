package admin

import (
	"github.com/gin-gonic/gin"
)

// registerEndpointRoutes 注册 /admin/v1/endpoints CRUD。
//
// 多租户：所有路由用 ?tenant_id= query 参数指定（v0.1 默认 "default"）；
// POST/PUT body 也可以带 tenant_id，URL 优先。
func registerEndpointRoutes(api *gin.RouterGroup, s *EndpointStore) {
	api.GET("/endpoints", listEndpoints(s))
	api.POST("/endpoints", createEndpoint(s))
	api.GET("/endpoints/:id", getEndpoint(s))
	api.PUT("/endpoints/:id", updateEndpoint(s))
	api.DELETE("/endpoints/:id", deleteEndpoint(s))
}

func listEndpoints(s *EndpointStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID := tenantOrDefault(c.Query("tenant_id"))
		all, err := s.List(c.Request.Context(), tenantID)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		items := make([]endpointDTO, len(all))
		for i := range all {
			items[i] = epToDTO(&all[i])
		}
		c.JSON(200, gin.H{"items": items})
	}
}

func getEndpoint(s *EndpointStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID := tenantOrDefault(c.Query("tenant_id"))
		ep, err := s.GetByID(c.Request.Context(), tenantID, c.Param("id"))
		if err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, epToDTO(ep))
	}
}

func createEndpoint(s *EndpointStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		var dto endpointDTO
		if err := c.ShouldBindJSON(&dto); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		dto.TenantID = tenantOrDefault(dto.TenantID)
		ep := dtoToEp(dto)
		if err := s.Create(c.Request.Context(), ep); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, epToDTO(ep))
	}
}

func updateEndpoint(s *EndpointStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		var dto endpointDTO
		if err := c.ShouldBindJSON(&dto); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		dto.ID = c.Param("id")                               // URL 是 id 真相来源
		dto.TenantID = tenantOrDefault(c.Query("tenant_id")) // URL query 是 tenant 来源
		ep := dtoToEp(dto)
		if err := s.Update(c.Request.Context(), ep); err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, epToDTO(ep))
	}
}

func deleteEndpoint(s *EndpointStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID := tenantOrDefault(c.Query("tenant_id"))
		if err := s.Delete(c.Request.Context(), tenantID, c.Param("id")); err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.Status(204)
	}
}
