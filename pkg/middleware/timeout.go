package middleware

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
)

// Timeout 给请求 ctx 加截止时间；上游调用与 RC.Ctx 都会感知到。
//
// d <= 0 时返回 no-op，方便各 modality 文件无脑调用而不必判 0。
func Timeout(d time.Duration) gin.HandlerFunc {
	if d <= 0 {
		return passthrough
	}
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), d)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
