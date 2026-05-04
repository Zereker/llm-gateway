package middleware

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
)

// Timeout 给请求 ctx 加截止时间；上游调用与 RC.Ctx 都会感知到。
//
// **客户端可覆盖**：X-Gateway-Timeout: <duration string>（如 "30s", "5m"）。
// 只能往**更严**的方向覆盖（不能比 cfg 默认更松）；防恶意客户端写超长 timeout
// 占着上游连接不放。畸形 header 静默 fallback 到 cfg。
//
// d <= 0 时表示"不强制 timeout"——header 仍可启用一个；都没就 no-op。
func Timeout(defaultDur time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		d := defaultDur
		if hdr := c.GetHeader(HeaderGatewayTimeout); hdr != "" {
			if parsed, err := time.ParseDuration(hdr); err == nil && parsed > 0 {
				if defaultDur <= 0 || parsed < defaultDur {
					d = parsed
				}
			}
		}

		if d <= 0 {
			c.Next()
			return
		}

		ctx, cancel := context.WithTimeout(c.Request.Context(), d)
		defer cancel()

		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
