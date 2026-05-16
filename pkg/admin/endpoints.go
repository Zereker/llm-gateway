package admin

import (
	"strconv"

	"github.com/gin-gonic/gin"
)

// registerEndpointRoutes 注册 /admin/v1/endpoints CRUD。
//
// **v0.3 改动**：去 ?account_id=（endpoints 全局上游池）。BYOK 真做时再加。
//
//	GET    /endpoints                  列表（?name=foo 走单查）
//	POST   /endpoints                  创建
//	GET    /endpoints/:id              详情（按 BIGINT id）
//	PUT    /endpoints/:id              改字段
//	DELETE /endpoints/:id              软删
func registerEndpointRoutes(api *gin.RouterGroup, s *EndpointStore) {
	api.GET("/endpoints", listEndpoints(s))
	api.POST("/endpoints", createEndpoint(s))
	api.GET("/endpoints/:id", getEndpoint(s))
	api.PUT("/endpoints/:id", updateEndpoint(s))
	api.DELETE("/endpoints/:id", deleteEndpoint(s))
}

func listEndpoints(s *EndpointStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		// ?name=foo 走单查
		if name := c.Query("name"); name != "" {
			ep, err := s.GetByName(c.Request.Context(), name)
			if err != nil {
				c.JSON(404, gin.H{"error": err.Error()})
				return
			}
			c.JSON(200, gin.H{"items": []endpointDTO{epToDTO(ep)}})
			return
		}

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
		id, err := parseInt64Param(c, "id")
		if err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		ep, err := s.GetByID(c.Request.Context(), id)
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
		dto.ID = 0 // server-assigned；忽略客户端传值
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
		id, err := parseInt64Param(c, "id")
		if err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		var dto endpointDTO
		if err := c.ShouldBindJSON(&dto); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		dto.ID = id // URL 是 id 真相来源
		ep := dtoToEp(dto)
		if err := s.Update(c.Request.Context(), ep); err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		fresh, err := s.GetByID(c.Request.Context(), id)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, epToDTO(fresh))
	}
}

func deleteEndpoint(s *EndpointStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := parseInt64Param(c, "id")
		if err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if err := s.Delete(c.Request.Context(), id); err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.Status(204)
	}
}

// parseInt64Param 取 :name 参数解析成 int64；非法格式 / 0 都报 400。
func parseInt64Param(c *gin.Context, name string) (int64, error) {
	raw := c.Param(name)
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, paramError{name: name, raw: raw, cause: err}
	}
	if v <= 0 {
		return 0, paramError{name: name, raw: raw, cause: errInvalidID}
	}
	return v, nil
}

type paramError struct {
	name  string
	raw   string
	cause error
}

func (e paramError) Error() string {
	return "param " + e.name + "=" + e.raw + " invalid: " + e.cause.Error()
}

var errInvalidID = simpleErr("must be positive int64")

type simpleErr string

func (e simpleErr) Error() string { return string(e) }
