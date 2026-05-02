package middleware

import (
	"context"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// Credentials 从请求头提取的鉴权凭证。
type Credentials struct {
	APIKey      string            // "Authorization: Bearer xxx" 或 "X-API-Key: xxx" 提取
	BearerToken string            // JWT 形态时使用
	Headers     map[string]string // 完整透传，自定义实现可用
}

// IdentityProvider M2 Auth middleware 的依赖接口。
//
// 内置默认实现包含 APIKey（file / in-memory）和 JWT（HS256 / RS256）。
//
// Implementations MUST be safe for concurrent use（多 gin handler goroutine 同时调用）。
type IdentityProvider interface {
	Resolve(c context.Context, creds *Credentials) (*domain.UserIdentity, error)
}
