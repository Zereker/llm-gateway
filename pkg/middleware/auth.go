package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/metric"
	"github.com/zereker-labs/ai-gateway/pkg/repo"
)

// AuthDeps M2 Auth middleware 的依赖。
type AuthDeps struct {
	Provider repo.IdentityProvider
}

// Auth 是 M2：从 header 提取凭证 → 调 IdentityProvider → 写 rc.Identity。
//
// 失败行为（统一走 abort → M9 写出 JSON）：
//   - 缺凭证 → 401 / ErrPermanent / "missing credentials"
//   - Provider 返回错误 → 401 / ErrPermanent / "invalid credentials: <err>"
//
// 成功后：
//   - rc.Identity 字段全部填充
//   - rc.Logger 追加 "user_id" 字段，下游 middleware / handler 的日志自动带上
func Auth(deps AuthDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc := GetRequestContext(c)

		creds := extractCredentials(c)
		if creds == nil {
			metric.Inc("ai_gateway.auth.total", "result", "missing")
			abort(c, 401, domain.ErrPermanent, "missing credentials")
			return
		}

		u, err := deps.Provider.Resolve(rc.Ctx, creds)
		if err != nil {
			metric.Inc("ai_gateway.auth.total", "result", "invalid")
			abort(c, 401, domain.ErrPermanent, "invalid credentials: "+err.Error())
			return
		}

		rc.Identity = *u
		if rc.Logger != nil {
			rc.Logger = rc.Logger.With("user_id", u.UserID)
		}
		metric.Inc("ai_gateway.auth.total", "result", "ok")
		c.Next()
	}
}

// extractCredentials 从请求头提取 Credentials。
//
// 优先级（同字段被覆盖时后者胜）：
//  1. Authorization: Bearer xxx → BearerToken（兼容 OpenAI / Anthropic SDK）
//     若 X-API-Key 未设置，同时也填入 APIKey（OpenAI 习惯把 sk-xxx 放 Bearer）
//  2. X-API-Key: xxx → APIKey（覆盖上面 Bearer 同步过来的值）
//
// Headers 全量保留，便于自定义 Provider 用其他 header（如 X-User-Id 等）。
//
// 没有任何凭证时返回 nil。
func extractCredentials(c *gin.Context) *repo.Credentials {
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
			creds.APIKey = tok // OpenAI-style: APIKey lives in Bearer
		}
	}
	if k := c.GetHeader("X-API-Key"); k != "" {
		creds.APIKey = k // explicit X-API-Key overrides
	}

	if creds.APIKey == "" && creds.BearerToken == "" {
		return nil
	}
	return creds
}
