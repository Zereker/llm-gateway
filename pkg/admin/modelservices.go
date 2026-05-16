package admin

import (
	"github.com/gin-gonic/gin"
)

// registerModelServiceRoutes 注册 /admin/v1/modelservices CRUD。
//
// **v0.3 改动**：去 ?account_id= 参数（modelservices 全局 catalog）。
//
//	GET    /modelservices                  列表
//	POST   /modelservices                  创建
//	GET    /modelservices/:model           按 model 查
//	PUT    /modelservices/:model           改 service_id（model 是 UNIQUE 业务键不可改）
//	DELETE /modelservices/:model           软删
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
		c.JSON(201, msToDTO(m)) // 回填 ID + 三件套时间戳
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
		// 重读拿最新 updated_at
		fresh, err := s.GetByModel(c.Request.Context(), m.Model)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, msToDTO(fresh))
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
