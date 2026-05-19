package middleware

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/metric"
)

// IdentityProvider M2 Auth 依赖的凭证 → 身份解析 port。
//
// 接口是 middleware-owned；实现者（pkg/repo.SQLAPIKeyProvider 等）按自己的领域
// 写代码、顺便满足这个 port。SQL 装配的小适配层放在 cmd/gateway/middleware_adapters.go
// （避免 middleware → ratelimit → repo → middleware 的 import cycle）。
//
// Implementations MUST be safe for concurrent use（多 gin handler goroutine 同时调）。
type IdentityProvider interface {
	Resolve(ctx context.Context, creds *domain.Credentials) (*domain.UserIdentity, error)
}

// AuthOption 配置 Auth middleware。
//
// 走 otelgin v0.68.0 同款 interface-Option 模式：cfg 在 Auth() 启动期一次性 build，
// hot path 闭包持有 tracer，per-request 0 lookup。
type AuthOption interface {
	apply(*authConfig)
}

type authOptionFunc func(*authConfig)

func (f authOptionFunc) apply(c *authConfig) { f(c) }

// authConfig Auth middleware 私有配置。
type authConfig struct {
	provider IdentityProvider
}

// WithIdentityProvider 注入 IdentityProvider 实现。必填；缺则 Auth() 构造期 panic。
func WithIdentityProvider(p IdentityProvider) AuthOption {
	return authOptionFunc(func(c *authConfig) { c.provider = p })
}

// Auth 是 M2：从 header 提取凭证 → 调 IdentityProvider → 写 rc.Identity。
//
// 失败行为（统一走 abort → M9 写出 JSON）：
//   - 缺凭证 → 401 / ErrPermanent / "missing credentials"
//   - Provider 返回错误 → 401 / ErrPermanent / "invalid credentials: <err>"
//
// 成功后：
//   - rc.Identity 字段全部填充
//   - sub_account_id 写入 OTel baggage；trace.CtxHandler 让所有后续 log record 自动带 sub_account_id 字段
func Auth(opts ...AuthOption) gin.HandlerFunc {
	cfg := authConfig{}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	if cfg.provider == nil {
		panic("middleware.Auth: WithIdentityProvider required")
	}
	tracer := otel.GetTracerProvider().Tracer(ScopeName)

	return func(c *gin.Context) {
		ctx, span := tracer.Start(c.Request.Context(), "auth.lookup")
		defer span.End()
		c.Request = c.Request.WithContext(ctx)

		creds := extractCredentials(c)
		if creds == nil {
			metric.Inc(metric.AuthTotal, "result", "missing")
			abortWithCode(c, 401, domain.ErrPermanent, domain.ErrCodeUnauthorized, "missing credentials")
			return
		}

		u, err := cfg.provider.Resolve(ctx, creds)
		if err != nil {
			metric.Inc(metric.AuthTotal, "result", "invalid")
			abortWithCode(c, 401, domain.ErrPermanent, domain.ErrCodeUnauthorized, "invalid credentials: "+err.Error())
			return
		}

		rc := GetRequestContext(c)
		rc.Identity = *u
		if member, err := baggage.NewMember("sub_account_id", u.SubAccountID); err == nil {
			if newBag, err := baggage.FromContext(ctx).SetMember(member); err == nil {
				ctx = baggage.ContextWithBaggage(ctx, newBag)
				c.Request = c.Request.WithContext(ctx)
			}
		}

		metric.Inc(metric.AuthTotal, "result", "ok")
		c.Next()
	}
}

// extractCredentials 从请求头提取 Credentials。
//
// 优先级（同字段被覆盖时后者胜）：
//  1. Authorization: Bearer xxx → BearerToken（兼容 OpenAI / Anthropic SDK）
//     若 X-API-Key 未设置，同时也填入 APIKey
//  2. X-API-Key: xxx → APIKey（覆盖上面 Bearer 同步过来的值）
//
// 没有任何凭证时返回 nil。
func extractCredentials(c *gin.Context) *domain.Credentials {
	creds := &domain.Credentials{Headers: make(map[string]string, len(c.Request.Header))}
	for k, v := range c.Request.Header {
		if len(v) > 0 {
			creds.Headers[k] = v[0]
		}
	}

	if auth := c.GetHeader("Authorization"); auth != "" {
		if strings.HasPrefix(auth, "Bearer ") {
			tok := strings.TrimPrefix(auth, "Bearer ")
			creds.BearerToken = tok
			creds.APIKey = tok
		}
	}

	if k := c.GetHeader("X-API-Key"); k != "" {
		creds.APIKey = k
	}

	if creds.APIKey == "" && creds.BearerToken == "" {
		return nil
	}

	return creds
}
