package domain

import "errors"

// ErrInvalidCredentials is the sentinel for invalid credentials — key not
// found / disabled / expired / revoked / parent account disabled are all
// unified into this one class (**not subdivided**, to avoid giving credential
// stuffers an enumeration oracle).
//
// **Contract** (docs/01 §5 + §7): IdentityProvider.Resolve errors split into
// two categories:
//
//	errors.Is(err, ErrInvalidCredentials) → client problem → M2 returns 401
//	any other error                       → dependency failure (DB down, etc.) → M2 fail-closed returns 503
//
// Implementations (repo.SQLAPIKeyProvider, etc.) must wrap not-found style
// errors with fmt.Errorf("...: %w", ErrInvalidCredentials); raw SQL errors
// are passed through as-is (not wrapped with this sentinel).
var ErrInvalidCredentials = errors.New("invalid credentials")

// UserIdentity is the product of the M2 Auth middleware (the parent account +
// sub-account context obtained by looking up the credentials).
//
// **AccountID** is the parent account pin / billing subject; M5 uses it to
// determine model subscription, M6 uses it to match the parent-account-level
// quota policy.
//
// **QuotaPolicy dual-layer pointers**: the two policy layers are independent
// and stack; exceeding either layer's limit results in rejection.
// NULL = that layer is unlimited.
//
// Design principle (docs/06 §3): a pure business struct, no SQL tags, no
// Scanner/Valuer, does not import repo.
type UserIdentity struct {
	AccountID            string
	SubAccountID         string
	APIKeyID             string
	Group                string
	ExternalUser         bool
	AccountQuotaPolicyID *int64
	APIKeyQuotaPolicyID  *int64
}

// Credentials are the auth credentials extracted from the request headers;
// the input to IdentityProvider.Resolve.
type Credentials struct {
	APIKey      string            // extracted from "Authorization: Bearer xxx" or "X-API-Key: xxx"
	BearerToken string            // used for JWT form
	Headers     map[string]string // passed through in full
}
