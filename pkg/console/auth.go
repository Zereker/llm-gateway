package console

import (
	"crypto/sha256"
	"crypto/subtle"
	"strings"

	"github.com/gin-gonic/gin"
)

// adminAuth 是控制面的 bearer-token 鉴权中间件。
//
// **跟数据面完全两套**：数据面 M2 认的是 end-user 的 api_key（DB 查表）；控制面认
// 的是 operator 的 admin token（配置注入）。两个面不同端口、不同凭证、不同信任域——
// 这正是为什么控制面该独立进程、跑内网。
//
// **常量时间比较**：先把两边都 SHA-256 成定长再 subtle.ConstantTimeCompare，
// 避免长度 / 前缀的时序侧信道。Phase 4 换 OIDC / 真 RBAC 时替换本文件即可。
func adminAuth(tokens []string) gin.HandlerFunc {
	hashed := make([][32]byte, len(tokens))
	for i, t := range tokens {
		hashed[i] = sha256.Sum256([]byte(t))
	}
	return func(c *gin.Context) {
		got := bearerToken(c)
		if got == "" {
			abortError(c, 401, "unauthorized", "missing bearer token")
			return
		}
		gotSum := sha256.Sum256([]byte(got))
		ok := false
		for i := range hashed {
			if subtle.ConstantTimeCompare(gotSum[:], hashed[i][:]) == 1 {
				ok = true
			}
		}
		if !ok {
			abortError(c, 401, "unauthorized", "invalid admin token")
			return
		}
		c.Next()
	}
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
