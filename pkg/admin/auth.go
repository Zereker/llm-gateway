package admin

import (
	"github.com/gin-gonic/gin"
)

const adminTokenHeader = "X-Admin-Token"

// authMW 校验 X-Admin-Token header。
//
// 设计：token 是次要保险——admin 服务的主防线是网络隔离（生产应只绑定
// 内网 / loopback，不暴露到公网）。token 没配（""）时拒所有请求，
// 防止误把无鉴权的 admin 服务上线。
func authMW(expected string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if expected == "" {
			c.AbortWithStatusJSON(500, gin.H{"error": "admin token not configured"})
			return
		}
		if c.GetHeader(adminTokenHeader) != expected {
			c.AbortWithStatusJSON(401, gin.H{"error": "invalid admin token"})
			return
		}
		c.Next()
	}
}
