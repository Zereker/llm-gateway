package request

import "github.com/gin-gonic/gin"

// gin.Context 上的 key；私有化在本包内，外部统一通过 From / Attach 操作。
const ctxKey = "ai_gateway.request_context"

// Attach 将 *Context 挂到 *gin.Context 上。仅 M1 TraceContext middleware 调用。
func Attach(c *gin.Context, rc *Context) {
	c.Set(ctxKey, rc)
}

// From 从 *gin.Context 取出 *Context。
//
// 假设 M1 已注册并已执行（M1 是链中第一个 middleware，未注册即配置错误，启动自检会拦下）。
// 若取不到则 panic — 由 M9 Recover 兜底转 500。
func From(c *gin.Context) *Context {
	v, ok := c.Get(ctxKey)
	if !ok {
		panic("request.Context not set: M1 TraceContext middleware missing")
	}
	return v.(*Context)
}

// TryFrom 是 From 的安全版：取不到返回 nil，专供 Recover 等兜底场景使用。
func TryFrom(c *gin.Context) *Context {
	v, ok := c.Get(ctxKey)
	if !ok {
		return nil
	}
	rc, _ := v.(*Context)
	return rc
}
