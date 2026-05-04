package admin

import (
	"encoding/json"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"

	"github.com/zereker-labs/ai-gateway/pkg/repo"
)

// registerPricingRoutes 挂在 /admin/v1/modelservices/:model 子路径下：
//
//	GET  /modelservices/:model/prices            列历史版本（?rule_class= 默认 standard）
//	GET  /modelservices/:model/prices/active     当前 active 版本
//	POST /modelservices/:model/prices            发布新版本（admin 改价唯一入口）
//
// **没有 PUT/DELETE**——append-only 协议，改价靠 RotatePrice INSERT，绝不允许就地改。
//
// model 路径段是 model_services.model；handler 内先反查拿到 model_service_id 再操作 pricing。
func registerPricingRoutes(api *gin.RouterGroup, msStore *ModelServiceStore, pStore *PricingStore) {
	api.GET("/modelservices/:model/prices", listPricingHistory(msStore, pStore))
	api.GET("/modelservices/:model/prices/active", getActivePricing(msStore, pStore))
	api.POST("/modelservices/:model/prices", rotatePricing(msStore, pStore))
}

// resolveModelServiceID URL 路径上的 :model 反查全局 catalog ID。
//
// 没找到 → handler 自己写 404；有找到 → 返回 (id, true).
func resolveModelServiceID(c *gin.Context, msStore *ModelServiceStore) (int64, bool) {
	model := c.Param("model")
	ms, err := msStore.GetByModel(c.Request.Context(), model)
	if err != nil {
		c.JSON(404, gin.H{"error": "model_service not found: " + model})
		return 0, false
	}
	return ms.ID, true
}

func listPricingHistory(msStore *ModelServiceStore, pStore *PricingStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		msID, ok := resolveModelServiceID(c, msStore)
		if !ok {
			return
		}
		tenantID := tenantOrDefault(c.Query("tenant_id"))
		ruleClass := defaultIfEmpty(c.Query("rule_class"), "standard")

		all, err := pStore.ListHistory(c.Request.Context(), tenantID, msID, ruleClass)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		items := make([]pricingVersionDTO, len(all))
		for i := range all {
			items[i] = pricingToDTO(&all[i])
		}
		c.JSON(200, gin.H{"items": items})
	}
}

func getActivePricing(msStore *ModelServiceStore, pStore *PricingStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		msID, ok := resolveModelServiceID(c, msStore)
		if !ok {
			return
		}
		tenantID := tenantOrDefault(c.Query("tenant_id"))
		ruleClass := defaultIfEmpty(c.Query("rule_class"), "standard")

		pv, err := pStore.GetActive(c.Request.Context(), tenantID, msID, ruleClass)
		if err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, pricingToDTO(pv))
	}
}

func rotatePricing(msStore *ModelServiceStore, pStore *PricingStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		msID, ok := resolveModelServiceID(c, msStore)
		if !ok {
			return
		}
		var req pricingRotateRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		// json.RawMessage 经一轮 marshal/unmarshal 后，nil 会变成 "null"（长度 4）；
		// 两种空形态都拒。
		if len(req.RuleJSON) == 0 || string(req.RuleJSON) == "null" {
			c.JSON(400, gin.H{"error": "rule_json required"})
			return
		}
		tenantID := tenantOrDefault(c.Query("tenant_id"))
		ruleClass := defaultIfEmpty(req.RuleClass, "standard")

		pv, err := pStore.RotatePrice(c.Request.Context(), tenantID, msID, ruleClass,
			datatypes.JSON(req.RuleJSON), req.CreatedBy, req.Notes)
		if err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, pricingToDTO(pv))
	}
}

// =============================================================================
// DTO
// =============================================================================

type pricingVersionDTO struct {
	ID               int64           `json:"id"`
	TenantID         string          `json:"tenant_id"`
	ModelServiceID   int64           `json:"model_service_id"`
	RuleClass        string          `json:"rule_class"`
	EffectiveFrom    time.Time       `json:"effective_from"`
	EffectiveTo      *time.Time      `json:"effective_to,omitempty"`
	RuleJSON         json.RawMessage `json:"rule_json"`
	CreatedAt        time.Time       `json:"created_at"`
	CreatedBy        string          `json:"created_by,omitempty"`
	Notes            string          `json:"notes,omitempty"`
}

func pricingToDTO(p *repo.PricingVersion) pricingVersionDTO {
	return pricingVersionDTO{
		ID:             p.ID,
		TenantID:       p.TenantID,
		ModelServiceID: p.ModelServiceID,
		RuleClass:      p.RuleClass,
		EffectiveFrom:  p.EffectiveFrom,
		EffectiveTo:    p.EffectiveTo,
		RuleJSON:       jsonRawFromDatatype(p.RuleJSON),
		CreatedAt:      p.CreatedAt,
		CreatedBy:      p.CreatedBy,
		Notes:          p.Notes,
	}
}

// pricingRotateRequest POST /modelservices/:model/prices 请求体。
//
// rule_json 透传——admin 不解析，billing engine 当真相。
type pricingRotateRequest struct {
	RuleClass string          `json:"rule_class,omitempty"` // 默认 "standard"
	RuleJSON  json.RawMessage `json:"rule_json"`            // 必填
	CreatedBy string          `json:"created_by,omitempty"` // 操作员标识，审计用
	Notes     string          `json:"notes,omitempty"`      // 改价原因 / 契约编号
}
