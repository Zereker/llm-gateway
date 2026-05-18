package middleware

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// ModelCatalog M5 用：按 model 字符串查全局 catalog。
//
// 该接口是 middleware-owned 契约，repo 实现层把 SQL 行映射为 domain.ModelService。
type ModelCatalog interface {
	GetByModel(c context.Context, model string) (*domain.ModelService, error)
}

// SubscriptionChecker M5 用：判定主账号是否订阅了某 model_service。
type SubscriptionChecker interface {
	HasModel(c context.Context, accountID string, modelServiceID int64) (bool, error)
}

// ModelServiceOption 配置 ModelService middleware（otelgin v0.68.0 同款 interface-Option）。
type ModelServiceOption interface {
	apply(*modelServiceConfig)
}

type modelServiceOptionFunc func(*modelServiceConfig)

func (f modelServiceOptionFunc) apply(c *modelServiceConfig) { f(c) }

type modelServiceConfig struct {
	catalog       ModelCatalog
	subscriptions SubscriptionChecker
}

// WithModelCatalog 注入 ModelCatalog 实现。必填。
func WithModelCatalog(c ModelCatalog) ModelServiceOption {
	return modelServiceOptionFunc(func(cfg *modelServiceConfig) { cfg.catalog = c })
}

// WithSubscriptionChecker 注入 SubscriptionChecker 实现。必填。
func WithSubscriptionChecker(s SubscriptionChecker) ModelServiceOption {
	return modelServiceOptionFunc(func(cfg *modelServiceConfig) { cfg.subscriptions = s })
}

// ModelService 是 M5：rc.Envelope.Model → catalog → 验订阅 → rc.ModelService。
//
// 同时解析 X-Gateway-Fallback-Models（docs/03 §5）：把 primary + 已校验过的
// fallback model 写入 rc.ModelChain 供 M7 直接消费。fallback 不存在或未订阅
// 时静默剔除，不影响主请求。
//
// 失败行为（docs/01 §7）：
//   - rc.Envelope nil（M3 没跑）→ 500
//   - catalog SQL 错 → 503 / dependency_unavailable
//   - catalog 找不到 primary → 404 / model_not_found
//   - 主账号没订阅 primary → 403 / model_not_subscribed
//   - fallback 解析失败 → 静默剔除，不阻断（仅 primary 必须成功）
func ModelService(opts ...ModelServiceOption) gin.HandlerFunc {
	cfg := modelServiceConfig{}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	if cfg.catalog == nil {
		panic("middleware.ModelService: WithModelCatalog required")
	}
	if cfg.subscriptions == nil {
		panic("middleware.ModelService: WithSubscriptionChecker required")
	}
	tracer := otel.GetTracerProvider().Tracer(ScopeName)

	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		ctx, span := tracer.Start(rc.Ctx, "catalog.resolve")
		defer span.End()
		rc.Ctx = ctx

		if rc.Envelope == nil {
			abortWithCode(c, 500, domain.ErrUnknown, domain.ErrCodeInternalError,
				"internal: M3 Envelope did not run before M5")
			return
		}

		ms, err := cfg.catalog.GetByModel(rc.Ctx, rc.Envelope.Model)
		if err != nil {
			abortWithCode(c, 503, domain.ErrTransient, domain.ErrCodeDependencyUnavailable,
				"model catalog: "+err.Error())
			return
		}
		if ms == nil {
			abortWithCode(c, 404, domain.ErrInvalid, domain.ErrCodeModelNotFound,
				"model not found: "+rc.Envelope.Model)
			return
		}

		subscribed, err := cfg.subscriptions.HasModel(rc.Ctx, rc.Identity.AccountID, ms.ID)
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
		rc.ModelChain = resolveModelChain(rc.Ctx, cfg, ms, rc.Identity.AccountID,
			parseFallbackModels(c, rc.Envelope.Model))
		c.Next()
	}
}

// resolveModelChain 把 primary + 已校验过的 fallback 拼成 rc.ModelChain。
//
// fallback 路径上**任何**校验失败（catalog 找不到 / subs 拒绝 / 依赖故障）都只是
// 静默跳过该 fallback——primary 已经成功，不应该让 fallback 解析失败拖累主请求。
func resolveModelChain(
	ctx context.Context,
	cfg modelServiceConfig,
	primary *domain.ModelService,
	accountID string,
	fallbackModels []string,
) []*domain.ModelService {
	chain := make([]*domain.ModelService, 0, 1+len(fallbackModels))
	chain = append(chain, primary)

	if len(fallbackModels) == 0 {
		return chain
	}

	seen := map[string]struct{}{primary.Model: {}}
	for _, m := range fallbackModels {
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}

		ms, err := cfg.catalog.GetByModel(ctx, m)
		if err != nil || ms == nil {
			continue
		}
		subscribed, err := cfg.subscriptions.HasModel(ctx, accountID, ms.ID)
		if err != nil || !subscribed {
			continue
		}
		chain = append(chain, ms)
	}
	return chain
}

// parseFallbackModels 读 X-Gateway-Fallback-Models header（逗号分隔，去重保序）；
// docs/03 §5：去重保序、空 model 忽略、数量上限 MaxFallbackModels。
// primary 自身从结果里剔除——chain 里不允许它和 primary 同名。
func parseFallbackModels(c *gin.Context, primary string) []string {
	hdr := c.GetHeader(HeaderGatewayFallbackModels)
	if hdr == "" {
		return nil
	}
	seen := make(map[string]struct{}, MaxFallbackModels)
	out := make([]string, 0, MaxFallbackModels)
	for _, m := range strings.Split(hdr, ",") {
		m = strings.TrimSpace(m)
		if m == "" || m == primary {
			continue
		}
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
		if len(out) >= MaxFallbackModels {
			break
		}
	}
	return out
}

// 旧的 AdaptRepoCatalog / AdaptRepoSubscriptions 已迁到 cmd/gateway/middleware_adapters.go
// （adaptCatalog / adaptSubscriptions）；放在 composition root 是为了避免
// middleware → ratelimit → repo → middleware 的 import cycle。middleware 现在
// 不再 import pkg/repo。
