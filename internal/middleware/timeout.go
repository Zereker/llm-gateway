package middleware

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
)

// Timeout attaches a deadline to the request ctx; both the upstream call and
// RC.Ctx observe it.
//
// **Client-overridable**: X-Gateway-Timeout: <duration string> (e.g. "30s", "5m").
// Can only override in the **stricter** direction (never looser than the cfg
// default); prevents a malicious client from setting an oversized timeout to
// hog an upstream connection. A malformed header silently falls back to cfg.
//
// d <= 0 means "no enforced timeout" — the header can still enable one; if
// neither is set, it's a no-op.
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
