// Package middleware 实现请求生命周期的 10 个 middleware (M1-M10) + 注册装配 +
// RequestContext 存取 helper。
//
// 详见 docs/architecture/01-request-pipeline.md。
package middleware

import (
	"context"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// rcCtxKey 用 stdlib context.Value 的 typed-key 模式：私有 struct 类型作 key，
// 跟其它包的 ctx value 不会撞 key（哪怕字面 string 一样）。
//
// 不导出，外部一律通过 GetRequestContext / TryGetRequestContext / AttachRequestContext
// 间接访问，杜绝散落 c.Get("...") 用法。
type rcCtxKey struct{}

// requestContextKey 是 *domain.RequestContext 在 context.Context 上的 typed key。
var requestContextKey = rcCtxKey{}

// GetRequestContext 从 *gin.Context 取出 *RequestContext。
//
// **存储位置**：RequestContext 通过 stdlib `context.WithValue` 挂在
// `c.Request.Context()` 上（不是 gin 自家的 c.Set/c.Get map）；这跟 OTel
// SpanContext / Baggage 走同一个容器（ctx），整条链路只一种数据传递机制。
//
// 假设 M1 TraceContext 已注册并已执行；取不到则 panic（M9 Recover 兜底转 500）。
// 业务代码不应裸调 c.Request.Context().Value；统一走本函数。
func GetRequestContext(c *gin.Context) *domain.RequestContext {
	rc := fromCtx(c.Request.Context())
	if rc == nil {
		panic("RequestContext not set: M1 TraceContext middleware missing")
	}
	return rc
}

// TryGetRequestContext 是 GetRequestContext 的安全版：取不到返回 nil，
// 专供 M9 Recover 等兜底场景使用。
func TryGetRequestContext(c *gin.Context) *domain.RequestContext {
	if c == nil || c.Request == nil {
		return nil
	}
	return fromCtx(c.Request.Context())
}

// AttachRequestContext 将 *RequestContext 挂到 c.Request.Context()；仅 M1 TraceContext 调用。
//
// 调用后 `c.Request.Context()` 链路上多一个 WithValue 节点；rc.Ctx 字段同步更新
// 指向这个新 ctx（保证 rc.Ctx 跟 c.Request.Context() 一致）。
func AttachRequestContext(c *gin.Context, rc *domain.RequestContext) {
	ctx := context.WithValue(c.Request.Context(), requestContextKey, rc)
	c.Request = c.Request.WithContext(ctx)
	rc.Ctx = ctx
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

// abort 是早期 middleware（M2-M8）拒绝请求的统一出口：
//  1. 把 AdapterError 写到 rc.Error
//  2. c.Abort() 阻断后续 middleware
//  3. M9 Recover 在 defer 后看到 rc.Error 写出 JSON 响应
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
