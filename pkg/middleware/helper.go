// Package middleware 实现请求生命周期的 10 个 middleware (M1-M10) + 注册装配 +
// RequestContext 存取 helper。
//
// 详见 docs/architecture/01-request-pipeline.md。
package middleware

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// GetRequestContext 从 *gin.Context 取出 *RequestContext。
//
// 假设 M1 TraceContext 已注册并已执行；取不到则 panic（M9 Recover 兜底转 500）。
// 业务代码不应裸调 c.MustGet / c.Get；统一走本函数。
func GetRequestContext(c *gin.Context) *domain.RequestContext {
	v, ok := c.Get(domain.RequestContextKey)
	if !ok {
		panic("RequestContext not set: M1 TraceContext middleware missing")
	}
	return v.(*domain.RequestContext)
}

// TryGetRequestContext 是 GetRequestContext 的安全版：取不到返回 nil，
// 专供 M9 Recover 等兜底场景使用。
func TryGetRequestContext(c *gin.Context) *domain.RequestContext {
	v, ok := c.Get(domain.RequestContextKey)
	if !ok {
		return nil
	}
	rc, _ := v.(*domain.RequestContext)
	return rc
}

// AttachRequestContext 将 *RequestContext 挂到 *gin.Context；仅 M1 TraceContext 调用。
func AttachRequestContext(c *gin.Context, rc *domain.RequestContext) {
	c.Set(domain.RequestContextKey, rc)
}

// abort 是早期 middleware（M2-M8）拒绝请求的统一出口：
//   1. 把 AdapterError 写到 rc.Error
//   2. c.Abort() 阻断后续 middleware
//   3. M9 Recover 在 defer 后看到 rc.Error 写出 JSON 响应
//
// 这样所有早期拒绝走同一份"错误响应格式"，避免每个 middleware 自己 c.JSON。
//
// status == 0 时由 domain.DefaultHTTPStatus 按 class 推导。
func abort(c *gin.Context, status int, class domain.ErrorClass, message string) {
	if rc := TryGetRequestContext(c); rc != nil {
		rc.Error = &domain.AdapterError{
			Class:      class,
			HTTPStatus: status,
			Message:    message,
		}
	}
	c.Abort()
}
