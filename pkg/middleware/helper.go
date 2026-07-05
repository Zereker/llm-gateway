// Package middleware implements the 10 middlewares of the request lifecycle
// (M1-M10) + registration wiring + RequestContext access helpers.
//
// See docs/architecture/01-request-pipeline.md for details.
package middleware

import (
	"context"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// rcCtxKey uses the stdlib context.Value typed-key pattern: a private struct
// type as the key, so it never collides with ctx values from other packages
// (even if the literal string were the same).
type rcCtxKey struct{}

var requestContextKey = rcCtxKey{}

// GetRequestContext retrieves *RequestContext from *gin.Context.
//
// Assumes M1 TraceContext has already been registered and run; panics if not
// found (M9 Recover falls back to a 500).
func GetRequestContext(c *gin.Context) *domain.RequestContext {
	rc := fromCtx(c.Request.Context())
	if rc == nil {
		panic("RequestContext not set: M1 TraceContext middleware missing")
	}
	return rc
}

// AttachRequestContext attaches *RequestContext to c.Request.Context(); called
// only by M1 TraceContext.
//
// Afterward, any downstream middleware that needs RC goes through
// `GetRequestContext(c)`, and any that needs ctx goes through
// `c.Request.Context()` — single source of truth.
func AttachRequestContext(c *gin.Context, rc *domain.RequestContext) {
	ctx := context.WithValue(c.Request.Context(), requestContextKey, rc)
	c.Request = c.Request.WithContext(ctx)
}

// fromCtx is the internal typed-key extraction. Returns nil if ctx is nil or
// the key is absent.
func fromCtx(ctx context.Context) *domain.RequestContext {
	if ctx == nil {
		return nil
	}
	v := ctx.Value(requestContextKey)
	if v == nil {
		return nil
	}
	rc, _ := v.(*domain.RequestContext)
	return rc
}

// abort is the unified exit point for early middleware (M2-M8) rejecting a request.
//
// When status == 0, it's derived from domain.DefaultHTTPStatus by class; Code
// is derived from domain.DefaultCode by class.
//
// Use abortWithCode to customize Code.
func abort(c *gin.Context, status int, class domain.ErrorClass, message string) {
	abortWithCode(c, status, class, "", message)
}

// abortWithCode is the same as abort, but explicitly specifies a stable
// machine code (docs/01 §8).
//
// When code == "", it's derived from domain.DefaultCode by class.
func abortWithCode(c *gin.Context, status int, class domain.ErrorClass, code, message string) {
	rc := GetRequestContext(c)
	if code == "" {
		code = domain.DefaultCode(class)
	}
	rc.Error = &domain.AdapterError{
		Class:      class,
		Code:       code,
		HTTPStatus: status,
		Message:    message,
	}
	c.Abort()
}

// abortWithDetails is the same as abortWithCode + extra troubleshooting fields
// (rate-limit dimension / endpoint_id, etc.).
func abortWithDetails(c *gin.Context, status int, class domain.ErrorClass, code, message string, details map[string]any) {
	rc := GetRequestContext(c)
	if code == "" {
		code = domain.DefaultCode(class)
	}
	rc.Error = &domain.AdapterError{
		Class:      class,
		Code:       code,
		HTTPStatus: status,
		Message:    message,
		Details:    details,
	}
	c.Abort()
}
