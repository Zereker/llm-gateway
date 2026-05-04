package middleware

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/repo"
)

// ModelServiceDeps M5 ModelService middleware 的依赖。
//
// **v0.3 改动**：加 Subscriptions 依赖。M5 三步走：
//  1. 模型在全局 catalog？
//  2. 当前 tenant 订阅了？
//  3. tenant 维度有 active price？
type ModelServiceDeps struct {
	Provider      repo.ModelServiceReader
	Subscriptions repo.SubscriptionProvider
	Pricing       repo.PricingProvider
}

// ModelService 是 M5：根据 rc.Envelope.Model 定位 ModelService → 验订阅 → 拍价格快照。
//
// 失败行为：
//   - rc.Envelope 为 nil（M3 顺序错） → 500（应该早期 panic / fail-fast）
//   - 模型未注册（catalog 没这个 model） → 404 / ErrInvalid / "model not found"
//   - 模型存在但 tenant 没订阅 → 403 / ErrPermanent / "model not subscribed"
//   - 订阅了但 tenant 维度没 active price → 503 / ErrTransient / "no active version..."
//
// 成功后：
//   - rc.ModelService 字段就绪
//   - rc.Pricing 填入 (ModelServiceID, PricingVersionID, PricingEffectiveFrom, RuleClass) 指纹
//
// **rule_class 选择**：v0.3 全局硬编码 "standard"。后续 api_keys 加 pricing_class 列后，
// 在这里改成 `class := rc.Identity.PricingClass; if class == "" { class = "standard" }`。
const defaultRuleClass = "standard"

func ModelService(deps ModelServiceDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		ctx, end := startSpan(rc.Ctx, "ai-gateway.model_service")
		defer end()
		rc.Ctx = ctx
		if rc.Envelope == nil {
			abort(c, 500, domain.ErrUnknown, "internal: M3 Envelope did not run before M5")
			return
		}

		// Step 1: 全局 catalog 查 model
		ms, err := deps.Provider.GetByModel(rc.Ctx, rc.Envelope.Model)
		if err != nil {
			abort(c, 404, domain.ErrInvalid, "model not found: "+rc.Envelope.Model)
			return
		}

		// Step 2: 订阅 ACL
		subscribed, err := deps.Subscriptions.Has(rc.Ctx, rc.Identity.TenantID, ms.ID)
		if err != nil {
			abort(c, 500, domain.ErrUnknown, "subscription lookup: "+err.Error())
			return
		}

		if !subscribed {
			abort(c, 403, domain.ErrPermanent, "model not subscribed: "+rc.Envelope.Model)
			return
		}

		// Step 3: 价格快照
		pv, err := deps.Pricing.GetActive(rc.Ctx, rc.Identity.TenantID, ms.ID, defaultRuleClass, time.Now().UTC())
		if err != nil {
			abort(c, 503, domain.ErrTransient, err.Error())
			return
		}

		rc.ModelService = ms
		rc.Pricing = domain.PricingSnapshot{
			ModelServiceID:       ms.ID,
			PricingVersionID:     pv.ID,
			PricingEffectiveFrom: pv.EffectiveFrom,
			RuleClass:            defaultRuleClass,
		}
		c.Next()
	}
}
