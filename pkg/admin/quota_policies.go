package admin

import (
	"encoding/json"

	"github.com/gin-gonic/gin"
)

// registerQuotaPolicyRoutes 注册 /admin/v1/quota-policies CRUD。
//
//	GET    /quota-policies            列表
//	POST   /quota-policies            创建
//	GET    /quota-policies/:id        详情（按 BIGINT id；用 ?name= 走 GetByName）
//	PUT    /quota-policies/:id        改 description / rule_json / enabled
//	DELETE /quota-policies/:id        软删
//
// **可改的就地 UPDATE rule_json**——quota 调整即时生效，不像 pricing 是 append-only。
// 如果 admin 想保留改价历史，得在外部走自己的审计日志。
func registerQuotaPolicyRoutes(api *gin.RouterGroup, s *QuotaPolicyStore) {
	api.GET("/quota-policies", listQuotaPolicies(s))
	api.POST("/quota-policies", createQuotaPolicy(s))
	api.GET("/quota-policies/:id", getQuotaPolicy(s))
	api.PUT("/quota-policies/:id", updateQuotaPolicy(s))
	api.DELETE("/quota-policies/:id", deleteQuotaPolicy(s))
}

func listQuotaPolicies(s *QuotaPolicyStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		// ?name=foo 走 GetByName 单查（admin UI 用名字找 ID）
		if name := c.Query("name"); name != "" {
			p, err := s.GetByName(c.Request.Context(), name)
			if err != nil {
				c.JSON(404, gin.H{"error": err.Error()})
				return
			}
			c.JSON(200, gin.H{"items": []quotaPolicyDTO{quotaPolicyToDTO(p)}})
			return
		}

		all, err := s.List(c.Request.Context())
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		items := make([]quotaPolicyDTO, len(all))
		for i := range all {
			items[i] = quotaPolicyToDTO(&all[i])
		}
		c.JSON(200, gin.H{"items": items})
	}
}

func getQuotaPolicy(s *QuotaPolicyStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := parseInt64Param(c, "id")
		if err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		p, err := s.GetByID(c.Request.Context(), id)
		if err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, quotaPolicyToDTO(p))
	}
}

func createQuotaPolicy(s *QuotaPolicyStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		var dto quotaPolicyDTO
		if err := c.ShouldBindJSON(&dto); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		// json.RawMessage 经一轮 marshal/unmarshal 后 nil 会变成 "null"
		if len(dto.RuleJSON) == 0 || string(dto.RuleJSON) == "null" {
			c.JSON(400, gin.H{"error": "rule_json required"})
			return
		}
		p := dtoToQuotaPolicy(dto)
		if err := s.Create(c.Request.Context(), p); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, quotaPolicyToDTO(p))
	}
}

func updateQuotaPolicy(s *QuotaPolicyStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := parseInt64Param(c, "id")
		if err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		var req quotaPolicyUpdateRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		var rulePtr *[]byte
		if len(req.RuleJSON) > 0 && string(req.RuleJSON) != "null" {
			rule := []byte(req.RuleJSON)
			rulePtr = &rule
		}
		updates := QuotaPolicyUpdates{
			Description: req.Description,
			RuleJSON:    rulePtr,
			Enabled:     req.Enabled,
		}
		if err := s.Update(c.Request.Context(), id, updates); err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		p, err := s.GetByID(c.Request.Context(), id)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, quotaPolicyToDTO(p))
	}
}

func deleteQuotaPolicy(s *QuotaPolicyStore) gin.HandlerFunc {
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

// quotaPolicyUpdateRequest PUT body；nil = 不动。
type quotaPolicyUpdateRequest struct {
	Description *string         `json:"description,omitempty"`
	RuleJSON    json.RawMessage `json:"rule_json,omitempty"`
	Enabled     *bool           `json:"enabled,omitempty"`
}
