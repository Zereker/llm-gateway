package console

import (
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Role 是控制面 operator 的粗粒度权限（Phase 4 的 RBAC 基元）。
//
// Phase 4 只做两档；真正的多租户自助 + 细粒度 RBAC（per-tenant scoping / OIDC）
// 是更大的产品决策，留作后续。
type Role string

const (
	// RoleAdmin 全权：读 + 写（建/删/发 key/吊销）。
	RoleAdmin Role = "admin"
	// RoleViewer 只读：只能 GET。
	RoleViewer Role = "viewer"
)

// Token 一个 admin 凭证 + 它的角色 + 可选的可读名（审计里当 actor）。
type Token struct {
	Value string `yaml:"token"`
	Role  Role   `yaml:"role"`
	Name  string `yaml:"name"`
}

// ctxRoleKey / ctxActorKey gin.Context 里存角色 / actor 名的键。
const (
	ctxRoleKey  = "console_role"
	ctxActorKey = "console_actor"
)

// adminAuth 鉴权 bearer token，解析出角色写进 context。
//
// **常量时间**：把入参和每个已配 token 都 SHA-256 成定长再 ConstantTimeCompare，
// 且**遍历所有条目不早退**——不泄漏"匹配了哪个 token / 长度前缀"的时序信息。
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
			actor = string(role) // 没配 name 时用 role 兜底当 actor
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
		var role Role
		var actor string
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

// auditWrites 是 group 级中间件：写操作（POST/DELETE/PUT/PATCH）跑完后记一条审计。
// 挂在 adminAuth **之后**（actor / role 已入 context）。best-effort——审计写失败只
// warn，不影响已完成的操作。刻意不记 request body（见 audit_log schema）。
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

// requireAdmin 是写操作的守卫：非 admin 角色 → 403。挂在 POST/DELETE 路由上，
// 让 viewer token 只能读。
func requireAdmin(c *gin.Context) {
	if c.GetString(ctxRoleKey) != string(RoleAdmin) {
		abortError(c, 403, "forbidden", "admin role required for this operation")
		return
	}
	c.Next()
}

// bearerToken 从 Authorization: Bearer <t> 抽 token（不区分大小写的 scheme）。
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
