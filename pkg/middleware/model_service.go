package middleware

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/repo"
)

// ModelServiceDeps M5 ModelService middleware 的依赖。
type ModelServiceDeps struct {
	Provider repo.ModelServiceReader
}

// ModelService 是 M5：根据 rc.Envelope.Parsed.Model 加载 ModelServiceSnapshot + Pricing 指纹。
//
// 失败行为：
//   - rc.Envelope 为 nil（M3 顺序错） → 500（应该早期 panic / fail-fast）
//   - 模型未注册 → 404 / ErrInvalid / "model not found: <name>"
//
// 成功后：
//   - rc.ModelService 字段就绪
//   - rc.Pricing 填入 (ModelServiceID, ServiceUpdateTime) 指纹
func ModelService(deps ModelServiceDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		if rc.Envelope == nil {
			abort(c, 500, domain.ErrUnknown, "internal: M3 Envelope did not run before M5")
			return
		}

		ms, err := deps.Provider.GetByModel(rc.Ctx, rc.Identity.TenantID, rc.Envelope.Parsed.Model)
		if err != nil {
			abort(c, 404, domain.ErrInvalid, "model not found: "+rc.Envelope.Parsed.Model)
			return
		}

		rc.ModelService = ms
		rc.Pricing = domain.PricingSnapshot{
			ModelServiceID:    ms.ID,
			ServiceUpdateTime: ms.UpdateTime,
		}
		c.Next()
	}
}
