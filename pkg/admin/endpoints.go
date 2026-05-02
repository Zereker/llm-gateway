package admin

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/repo"
)

// registerEndpointRoutes 注册 /admin/v1/endpoints CRUD。
//
// 跟 modelservices 同形态，但按业务 ID（caller 提供的字符串如 "openai_main"）做主键。
func registerEndpointRoutes(api *gin.RouterGroup, r repo.EndpointRepository) {
	api.GET("/endpoints", listEndpoints(r))
	api.POST("/endpoints", createEndpoint(r))
	api.GET("/endpoints/:id", getEndpoint(r))
	api.PUT("/endpoints/:id", updateEndpoint(r))
	api.DELETE("/endpoints/:id", deleteEndpoint(r))
}

func listEndpoints(r repo.EndpointReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		all, err := r.List(c.Request.Context())
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		items := make([]endpointDTO, len(all))
		for i := range all {
			items[i] = epToDTO(all[i])
		}
		c.JSON(200, gin.H{"items": items})
	}
}

func getEndpoint(r repo.EndpointReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		ep, err := r.GetByID(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, epToDTO(ep))
	}
}

func createEndpoint(r repo.EndpointWriter) gin.HandlerFunc {
	return func(c *gin.Context) {
		var dto endpointDTO
		if err := c.ShouldBindJSON(&dto); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		ep := dtoToEp(dto)
		if err := r.Create(c.Request.Context(), ep); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, epToDTO(ep))
	}
}

func updateEndpoint(r repo.EndpointWriter) gin.HandlerFunc {
	return func(c *gin.Context) {
		var dto endpointDTO
		if err := c.ShouldBindJSON(&dto); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		dto.ID = c.Param("id") // URL 是 id 真相来源
		ep := dtoToEp(dto)
		if err := r.Update(c.Request.Context(), ep); err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, epToDTO(ep))
	}
}

func deleteEndpoint(r repo.EndpointWriter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := r.Delete(c.Request.Context(), c.Param("id")); err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.Status(204)
	}
}
