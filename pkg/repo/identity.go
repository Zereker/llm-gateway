package repo

import (
	"context"
)

// IdentityProvider M2 Auth middleware 的依赖接口。
//
// 内置默认实现包含 APIKey（file / in-memory）和 JWT（HS256 / RS256）。
//
// Implementations MUST be safe for concurrent use（多 gin handler goroutine 同时调用）。
type IdentityProvider interface {
	Resolve(c context.Context, creds *Credentials) (*UserIdentity, error)
}
