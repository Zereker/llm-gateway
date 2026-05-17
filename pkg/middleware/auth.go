package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/baggage"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/metric"
	"github.com/zereker/llm-gateway/pkg/repo"
)

// AuthOption 配置 Auth middleware。
//
// 用 functional options 模式：每个依赖一个 With* 构造器。
// 缺必填依赖时构造期 panic（fail-fast 暴露装配错）。
type AuthOption func(*authConfig)

// authConfig Auth middleware 私有配置。
type authConfig struct {
	provider repo.IdentityProvider
}

// WithIdentityProvider 注入 IdentityProvider 实现。
//
// 必填；缺则 Auth() 构造期 panic。
func WithIdentityProvider(p repo.IdentityProvider) AuthOption {
	return func(c *authConfig) { c.provider = p }
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
	cfg := &authConfig{}
	for _, opt := range opts {
		opt(cfg)
	}
	if cfg.provider == nil {
		panic("middleware.Auth: WithIdentityProvider required")
	}

	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		ctx, end := startSpan(rc.Ctx, "auth.lookup")
		defer end()
		rc.Ctx = ctx

		creds := extractCredentials(c)
		if creds == nil {
			metric.Inc(metric.AuthTotal, "result", "missing")
			abortWithCode(c, 401, domain.ErrPermanent, domain.ErrCodeUnauthorized, "missing credentials")
			return
		}

		u, err := cfg.provider.Resolve(rc.Ctx, creds)
		if err != nil {
			metric.Inc(metric.AuthTotal, "result", "invalid")
			abortWithCode(c, 401, domain.ErrPermanent, domain.ErrCodeUnauthorized, "invalid credentials: "+err.Error())
			return
		}

		rc.Identity = *u
		if member, err := baggage.NewMember("sub_account_id", u.SubAccountID); err == nil {
			if newBag, err := baggage.FromContext(rc.Ctx).SetMember(member); err == nil {
				rc.Ctx = baggage.ContextWithBaggage(rc.Ctx, newBag)
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
	creds := &repo.Credentials{Headers: make(map[string]string, len(c.Request.Header))}
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
