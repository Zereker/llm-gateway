package admin

import (
	"github.com/gin-gonic/gin"
)

// registerEndpointRoutes 注册 /admin/v1/endpoints CRUD。
//
// 跟 modelservices 同形态，但按业务 ID（caller 提供的字符串如 "openai_main"）做主键。
func registerEndpointRoutes(api *gin.RouterGroup, s *EndpointStore) {
	api.GET("/endpoints", listEndpoints(s))
	api.POST("/endpoints", createEndpoint(s))
	api.GET("/endpoints/:id", getEndpoint(s))
	api.PUT("/endpoints/:id", updateEndpoint(s))
	api.DELETE("/endpoints/:id", deleteEndpoint(s))
}

func listEndpoints(s *EndpointStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		all, err := s.List(c.Request.Context())
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
		ep, err := s.GetByID(c.Request.Context(), c.Param("id"))
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
		dto.ID = c.Param("id") // URL 是 id 真相来源
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
		if err := s.Delete(c.Request.Context(), c.Param("id")); err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.Status(204)
	}
}
