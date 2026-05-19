// Package middleware 实现请求生命周期的 10 个 middleware (M1-M10) + 注册装配 +
// RequestContext 存取 helper。
//
// 详见 docs/architecture/01-request-pipeline.md。
package middleware

import (
	"context"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// rcCtxKey 用 stdlib context.Value 的 typed-key 模式：私有 struct 类型作 key，
// 跟其它包的 ctx value 不会撞 key（哪怕字面 string 一样）。
type rcCtxKey struct{}

var requestContextKey = rcCtxKey{}

// GetRequestContext 从 *gin.Context 取出 *RequestContext。
//
// 假设 M1 TraceContext 已注册并已执行；取不到则 panic（M9 Recover 兜底转 500）。
func GetRequestContext(c *gin.Context) *domain.RequestContext {
	rc := fromCtx(c.Request.Context())
	if rc == nil {
		panic("RequestContext not set: M1 TraceContext middleware missing")
	}
	return rc
}

// AttachRequestContext 将 *RequestContext 挂到 c.Request.Context()；仅 M1 TraceContext 调用。
//
// 之后任何下游 middleware 取 RC 都走 `GetRequestContext(c)`，取 ctx 都走
// `c.Request.Context()`——单源真相。
func AttachRequestContext(c *gin.Context, rc *domain.RequestContext) {
	ctx := context.WithValue(c.Request.Context(), requestContextKey, rc)
	c.Request = c.Request.WithContext(ctx)
}

// fromCtx 内部 typed-key 提取。ctx 为 nil 或 key 不存在返 nil。
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

// abort 是早期 middleware（M2-M8）拒绝请求的统一出口。
//
// status == 0 时由 domain.DefaultHTTPStatus 按 class 推导；Code 由
// domain.DefaultCode 按 class 推导。
//
// 想自定义 Code 用 abortWithCode。
func abort(c *gin.Context, status int, class domain.ErrorClass, message string) {
	abortWithCode(c, status, class, "", message)
}

// abortWithCode 同 abort，但显式指定稳定机器码 code（docs/01 §8）。
//
// code == "" 时由 domain.DefaultCode 按 class 推导。
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

// abortWithDetails 同 abortWithCode + 额外排障字段（限流维度 / endpoint_id 等）。
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
