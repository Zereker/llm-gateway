package repo

import (
	"context"
)

// IdentityProvider is the dependency interface for M2 Auth middleware.
//
// Built-in default implementations include APIKey (file / in-memory) and JWT (HS256 / RS256).
//
// Implementations MUST be safe for concurrent use (called by multiple gin
// handler goroutines at once).
type IdentityProvider interface {
	Resolve(c context.Context, creds *Credentials) (*UserIdentity, error)
}
