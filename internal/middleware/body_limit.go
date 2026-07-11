package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// BodyLimit limits the request body size; once the limit is exceeded, reading
// to EOF triggers a 413 via http.MaxBytesReader.
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
