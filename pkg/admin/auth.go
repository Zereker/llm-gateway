package admin

import (
	"crypto/subtle"

	"github.com/gin-gonic/gin"
)

const adminTokenHeader = "X-Admin-Token"

// authMW 校验 X-Admin-Token header。
//
// 设计：token 是次要保险——admin 服务的主防线是网络隔离（生产应只绑定
// 内网 / loopback，不暴露到公网）。token 没配（""）时拒所有请求，
// 防止误把无鉴权的 admin 服务上线。
//
// **timing-safe 比较**：用 subtle.ConstantTimeCompare 避免短/长 token 不同 byte
// 比较时间泄漏 token 长度信息。本地 internal 服务 timing attack 实操可能性低，
// 但写正确没成本。
func authMW(expected string) gin.HandlerFunc {
	expectedBytes := []byte(expected)
	return func(c *gin.Context) {
		if expected == "" {
			c.AbortWithStatusJSON(500, gin.H{"error": "admin token not configured"})
			return
		}
		got := []byte(c.GetHeader(adminTokenHeader))
		if subtle.ConstantTimeCompare(got, expectedBytes) != 1 {
			c.AbortWithStatusJSON(401, gin.H{"error": "invalid admin token"})
			return
		}
		c.Next()
	}
}
