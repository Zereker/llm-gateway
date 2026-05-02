package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// BodyLimit 限制请求体大小；超限读到 EOF 后由 http.MaxBytesReader 触发 413。
//
// maxBytes <= 0 时返回 no-op，方便各 modality 文件无脑调用而不必判 0。
func BodyLimit(maxBytes int64) gin.HandlerFunc {
	if maxBytes <= 0 {
		return passthrough
	}
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		c.Next()
	}
}

// passthrough 是不做任何事的 middleware，让 0-value 配置可以无差别注册。
func passthrough(c *gin.Context) { c.Next() }
