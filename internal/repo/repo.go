// Package repo is the data access layer: it abstracts "the data middleware needs
// to look up on the request path" into interfaces, with default implementations
// (file-backed / KV-backed) also living in this package.
//
// Scope — only interfaces of the "look up a record by key" shape:
//   - IdentityProvider     looks up UserIdentity by credential
//   - ModelServiceProvider looks up ModelServiceSnapshot by model
//   - EndpointProvider     selects an Endpoint by model + group
//
// Out of scope for this package:
//   - Detector / Parser        pure parsing logic (internal/middleware)
//   - Moderator / BudgetGate   external policy calls, not data lookups (internal/middleware)
//
// middleware declares its dependencies via repo.XxxProvider types; the concrete
// implementation is assembled and injected by cmd.
package repo
