package console

import (
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Role is the coarse-grained permission level for a control-plane operator
// (the RBAC primitive for Phase 4).
//
// Phase 4 only has two tiers; true multi-tenant self-service + fine-grained
// RBAC (per-tenant scoping / OIDC) is a bigger product decision left for
// later.
type Role string

const (
	// RoleAdmin has full power: read + write (create/delete/issue key/revoke).
	RoleAdmin Role = "admin"
	// RoleViewer is read-only: GET only.
	RoleViewer Role = "viewer"
)

// Token is an admin credential plus its role and an optional human-readable
// name (used as the actor in audit records).
type Token struct {
	Value string `yaml:"token"`
	Role  Role   `yaml:"role"`
	Name  string `yaml:"name"`
}

// ctxRoleKey / ctxActorKey are the gin.Context keys for the stored role / actor name.
const (
	ctxRoleKey  = "console_role"
	ctxActorKey = "console_actor"
)

// adminAuth authenticates the bearer token and writes the resolved role into
// the context.
//
// **Constant-time**: both the incoming value and every configured token are
// SHA-256'd to a fixed length before ConstantTimeCompare, and **all entries
// are iterated without an early exit** — this avoids leaking timing
// information about which token matched or any length prefix.
func adminAuth(tokens []Token) gin.HandlerFunc {
	type entry struct {
		sum   [32]byte
		role  Role
		actor string
	}

	entries := make([]entry, len(tokens))
	for i, t := range tokens {
		role := t.Role
		if role == "" {
			role = RoleAdmin
		}

		actor := t.Name
		if actor == "" {
			actor = string(role) // fall back to the role as the actor when name isn't configured
		}

		entries[i] = entry{sum: sha256.Sum256([]byte(t.Value)), role: role, actor: actor}
	}

	return func(c *gin.Context) {
		got := bearerToken(c)
		if got == "" {
			abortError(c, 401, "unauthorized", "missing bearer token")
			return
		}

		gotSum := sha256.Sum256([]byte(got))
		matched := false

		var (
			role  Role
			actor string
		)
		for _, e := range entries {
			if subtle.ConstantTimeCompare(gotSum[:], e.sum[:]) == 1 {
				matched = true
				role = e.role
				actor = e.actor
			}
		}

		if !matched {
			abortError(c, 401, "unauthorized", "invalid admin token")
			return
		}

		c.Set(ctxRoleKey, string(role))
		c.Set(ctxActorKey, actor)
		c.Next()
	}
}

// auditWrites is a group-level middleware: after a write operation
// (POST/DELETE/PUT/PATCH) completes, it records an audit entry. It is
// attached **after** adminAuth (actor / role are already in the context).
// Best-effort — a failed audit write only logs a warning and does not affect
// the already-completed operation. The request body is deliberately not
// recorded (see the audit_log schema).
func auditWrites(store *Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()

		switch c.Request.Method {
		case http.MethodPost, http.MethodDelete, http.MethodPut, http.MethodPatch:
			if err := store.RecordAudit(c.Request.Context(),
				c.GetString(ctxActorKey), c.GetString(ctxRoleKey),
				c.Request.Method, c.Request.URL.Path, c.Writer.Status()); err != nil {
				slog.WarnContext(c.Request.Context(), "audit record failed", "err", err,
					"method", c.Request.Method, "path", c.Request.URL.Path)
			}
		}
	}
}

// requireAdmin guards write operations: a non-admin role gets 403. It is
// attached to POST/DELETE routes so that a viewer token can only read.
func requireAdmin(c *gin.Context) {
	if c.GetString(ctxRoleKey) != string(RoleAdmin) {
		abortError(c, 403, "forbidden", "admin role required for this operation")
		return
	}

	c.Next()
}

// bearerToken extracts the token from an Authorization: Bearer <t> header
// (the scheme is matched case-insensitively).
func bearerToken(c *gin.Context) string {
	h := c.GetHeader("Authorization")
	if h == "" {
		return ""
	}

	const p = "bearer "
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return ""
	}

	return strings.TrimSpace(h[len(p):])
}
