package repo

import "github.com/zereker/llm-gateway/internal/domain"

// These types have migrated to internal/domain (docs/06 §3: domain is the source
// of truth for business structs; repo only provides the SQL implementation).
// The type aliases are kept so internal code like SQLAPIKeyProvider.Resolve
// can keep returning the original names.
//
// **New code should use domain.UserIdentity / domain.Credentials**; these
// aliases exist only as an internal transition.
type (
	UserIdentity = domain.UserIdentity
	Credentials  = domain.Credentials
)
