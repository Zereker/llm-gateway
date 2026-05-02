package admin

import (
	"github.com/gin-gonic/gin"
)

// registerModelServiceRoutes 注册 /admin/v1/modelservices CRUD。
//
// 路径设计：
//
//	GET    /modelservices         列表
//	POST   /modelservices         创建（body: modelServiceDTO）
//	GET    /modelservices/:model  按 model 字段查
//	PUT    /modelservices/:model  全量更新（URL 是 model 来源，body.Model 被覆盖）
//	DELETE /modelservices/:model  删除
func registerModelServiceRoutes(api *gin.RouterGroup, s *ModelServiceStore) {
	api.GET("/modelservices", listModelServices(s))
	api.POST("/modelservices", createModelService(s))
	api.GET("/modelservices/:model", getModelService(s))
	api.PUT("/modelservices/:model", updateModelService(s))
	api.DELETE("/modelservices/:model", deleteModelService(s))
}

func listModelServices(s *ModelServiceStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		all, err := s.List(c.Request.Context())
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		items := make([]modelServiceDTO, len(all))
		for i := range all {
			items[i] = msToDTO(&all[i])
		}
		c.JSON(200, gin.H{"items": items})
	}
}

func getModelService(s *ModelServiceStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		ms, err := s.GetByModel(c.Request.Context(), c.Param("model"))
		if err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, msToDTO(ms))
	}
}

func createModelService(s *ModelServiceStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		var dto modelServiceDTO
		if err := c.ShouldBindJSON(&dto); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		m := dtoToMS(dto)
		if err := s.Create(c.Request.Context(), m); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, msToDTO(m)) // 回填 ID + UpdateTime
	}
}

func updateModelService(s *ModelServiceStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		var dto modelServiceDTO
		if err := c.ShouldBindJSON(&dto); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		dto.Model = c.Param("model") // URL 是 model 真相来源
		m := dtoToMS(dto)
		if err := s.Update(c.Request.Context(), m); err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, msToDTO(m))
	}
}

func deleteModelService(s *ModelServiceStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := s.Delete(c.Request.Context(), c.Param("model")); err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.Status(204)
	}
}
