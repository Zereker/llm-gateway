package middleware

import (
	"context"
	"errors"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/repo"
)

// ModelServiceDeps M5 ModelService middleware 的依赖。
//
// 按 docs/01 §7 + docs/03 §1：M5 只做 catalog + subscription 校验；
// **不**查 active pricing（pricing 匹配下放给计费平台）。
type ModelServiceDeps struct {
	Catalog       ModelCatalog
	Subscriptions SubscriptionChecker
}

// ModelCatalog M5 用：按 model 字符串查全局 catalog。
//
// 该接口是 middleware 自有契约，repo 实现层把 SQL 行映射为 domain.ModelService。
type ModelCatalog interface {
	GetByModel(c context.Context, model string) (*domain.ModelService, error)
}

// SubscriptionChecker M5 用：判定主账号是否订阅了某 model_service。
type SubscriptionChecker interface {
	HasModel(c context.Context, accountID string, modelServiceID int64) (bool, error)
}

// ErrModelNotFound catalog 查不到模型。
var ErrModelNotFound = errors.New("model not found")

// ModelService 是 M5：rc.Envelope.Model → catalog → 验订阅 → rc.ModelService。
//
// 失败行为：
//   - rc.Envelope nil（M3 没跑）→ 500 / ErrUnknown
//   - catalog 找不到 → 404 / ErrInvalid / "model not found"
//   - 主账号没订阅 → 403 / ErrPermanent / "model not subscribed"
//   - SQL 错误 → 503 / ErrTransient（依赖故障 fail-closed，docs/01 §7）
//
// 成功后：rc.ModelService 字段就绪。
func ModelService(deps ModelServiceDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		ctx, end := startSpan(rc.Ctx, "llm-gateway.model_service")
		defer end()
		rc.Ctx = ctx

		if rc.Envelope == nil {
			abortWithCode(c, 500, domain.ErrUnknown, domain.ErrCodeInternalError,
				"internal: M3 Envelope did not run before M5")
			return
		}

		// Step 1: catalog lookup
		ms, err := deps.Catalog.GetByModel(rc.Ctx, rc.Envelope.Model)
		if err != nil {
			// docs/01 §7：SQL 错按依赖故障 fail-closed → 503，不能伪装成 404
			abortWithCode(c, 503, domain.ErrTransient, domain.ErrCodeDependencyUnavailable,
				"model catalog: "+err.Error())
			return
		}
		if ms == nil {
			abortWithCode(c, 404, domain.ErrInvalid, domain.ErrCodeModelNotFound,
				"model not found: "+rc.Envelope.Model)
			return
		}

		// Step 2: subscription
		subscribed, err := deps.Subscriptions.HasModel(rc.Ctx, rc.Identity.AccountID, ms.ID)
		if err != nil {
			abortWithCode(c, 503, domain.ErrTransient, domain.ErrCodeDependencyUnavailable,
				"subscription lookup: "+err.Error())
			return
		}
		if !subscribed {
			abortWithCode(c, 403, domain.ErrPermanent, domain.ErrCodeModelNotSubscribed,
				"model not subscribed: "+rc.Envelope.Model)
			return
		}

		rc.ModelService = ms
		c.Next()
	}
}

// =============================================================================
// repo adapter: 把现有 repo Provider 适配到 middleware-owned 接口
// =============================================================================

// AdaptRepoCatalog 把 repo.ModelServiceReader 适配为 ModelCatalog。
// 当 repo.GetByModel 返回 (nil, err) 时按"not found"语义：err 类型由 repo 决定，
// 网关层统一用 SQL err = 依赖故障，"无数据"由 repo 转 nil 返回。
func AdaptRepoCatalog(p repo.ModelServiceReader) ModelCatalog {
	return repoCatalogAdapter{p: p}
}

type repoCatalogAdapter struct{ p repo.ModelServiceReader }

func (a repoCatalogAdapter) GetByModel(ctx context.Context, model string) (*domain.ModelService, error) {
	return a.p.GetByModel(ctx, model)
}

// AdaptRepoSubscriptions 把 repo.SubscriptionProvider 适配为 SubscriptionChecker。
func AdaptRepoSubscriptions(p repo.SubscriptionProvider) SubscriptionChecker {
	return repoSubsAdapter{p: p}
}

type repoSubsAdapter struct{ p repo.SubscriptionProvider }

func (a repoSubsAdapter) HasModel(ctx context.Context, accountID string, modelServiceID int64) (bool, error) {
	return a.p.Has(ctx, accountID, modelServiceID)
}
