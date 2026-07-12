package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// BodyLimit limits the request body size via http.MaxBytesReader. The reader
// only errors the Read — it never writes a status itself; Envelope (M3, the
// single body consumer) recognizes *http.MaxBytesError and maps it to 413.
//
// Returns a no-op when maxBytes <= 0, so each modality file can call this
// unconditionally without checking for zero.
func BodyLimit(maxBytes int64) gin.HandlerFunc {
	if maxBytes <= 0 {
		return passthrough
	}

	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		c.Next()
	}
}

// passthrough is a middleware that does nothing, so zero-value configuration
// can still be registered uniformly.
func passthrough(c *gin.Context) { c.Next() }
