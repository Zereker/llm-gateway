package admin

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/repo"
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
func registerModelServiceRoutes(api *gin.RouterGroup, r repo.ModelServiceRepository) {
	api.GET("/modelservices", listModelServices(r))
	api.POST("/modelservices", createModelService(r))
	api.GET("/modelservices/:model", getModelService(r))
	api.PUT("/modelservices/:model", updateModelService(r))
	api.DELETE("/modelservices/:model", deleteModelService(r))
}

func listModelServices(r repo.ModelServiceReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		all, err := r.List(c.Request.Context())
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		items := make([]modelServiceDTO, len(all))
		for i := range all {
			items[i] = msToDTO(all[i])
		}
		c.JSON(200, gin.H{"items": items})
	}
}

func getModelService(r repo.ModelServiceReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		snap, err := r.GetByModel(c.Request.Context(), c.Param("model"))
		if err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, msToDTO(snap))
	}
}

func createModelService(r repo.ModelServiceWriter) gin.HandlerFunc {
	return func(c *gin.Context) {
		var dto modelServiceDTO
		if err := c.ShouldBindJSON(&dto); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		snap := dtoToMS(dto)
		if err := r.Create(c.Request.Context(), snap); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, msToDTO(snap)) // 回填了 ID + UpdateTime
	}
}

func updateModelService(r repo.ModelServiceWriter) gin.HandlerFunc {
	return func(c *gin.Context) {
		var dto modelServiceDTO
		if err := c.ShouldBindJSON(&dto); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		dto.Model = c.Param("model") // URL 是 model 字段的真相来源
		snap := dtoToMS(dto)
		if err := r.Update(c.Request.Context(), snap); err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, msToDTO(snap))
	}
}

func deleteModelService(r repo.ModelServiceWriter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := r.Delete(c.Request.Context(), c.Param("model")); err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.Status(204)
	}
}
