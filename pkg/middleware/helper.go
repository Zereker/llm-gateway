// Package middleware 实现请求生命周期的 10 个 middleware (M1-M10) + 注册装配 +
// RequestContext 存取 helper。
//
// 详见 docs/architecture/01-request-pipeline.md。
package middleware

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/ctx"
)

// GetRequestContext 从 *gin.Context 取出 *RequestContext。
//
// 假设 M1 TraceContext 已注册并已执行；取不到则 panic（M9 Recover 兜底转 500）。
// 业务代码不应裸调 c.MustGet / c.Get；统一走本函数。
func GetRequestContext(c *gin.Context) *ctx.RequestContext {
	v, ok := c.Get(ctx.RequestContextKey)
	if !ok {
		panic("RequestContext not set: M1 TraceContext middleware missing")
	}
	return v.(*ctx.RequestContext)
}

// TryGetRequestContext 是 GetRequestContext 的安全版：取不到返回 nil，
// 专供 M9 Recover 等兜底场景使用。
func TryGetRequestContext(c *gin.Context) *ctx.RequestContext {
	v, ok := c.Get(ctx.RequestContextKey)
	if !ok {
		return nil
	}
	rc, _ := v.(*ctx.RequestContext)
	return rc
}

// AttachRequestContext 将 *RequestContext 挂到 *gin.Context；仅 M1 TraceContext 调用。
func AttachRequestContext(c *gin.Context, rc *ctx.RequestContext) {
	c.Set(ctx.RequestContextKey, rc)
}
