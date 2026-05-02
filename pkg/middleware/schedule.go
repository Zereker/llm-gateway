package middleware

import (
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/adapter"
	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/repo"
)

// ScheduleDeps M7 Schedule middleware 的依赖。
type ScheduleDeps struct {
	Endpoints  repo.EndpointReader
	GetFactory func(vendor string) adapter.Factory // nil = 使用 adapter.Get
	HTTPClient *http.Client                        // nil = 使用 http.DefaultClient
}

// Schedule 是 M7（v0.1 简化版）：选 endpoint → 调 Adapter → 流式回写客户端。
//
// **v0.1 简化**：单次直连，无 RetryExecutor / Filter 链 / Cooldown / 切换。
// 完整 RetryExecutor + Filter 链由 pkg/schedule 提供，v0.5+ 接管 M7。
//
// 流程：
//   1. EndpointProvider.PickForModel → rc.Endpoint
//   2. adapter.Get(ep.Vendor) → adapter.Factory
//   3. Factory.NewSession → adapter.Session（defer Close）
//   4. Session.BuildRequest → *http.Request
//   5. HTTPClient.Do → 上游响应
//   6. 转发上游 status + headers，按 chunk Read upstream → Session.Feed → Write client
//   7. Session.Finalize → rc.Usage / rc.Error
//
// **响应已开始写后**，rc.Error 仍可设置但 M9 无法覆盖（Writer.Written() 检查），
// 这是流式架构的固有限制；M9 的 c.AbortWithStatusJSON 在已 Write 的响应上是 no-op。
func Schedule(deps ScheduleDeps) gin.HandlerFunc {
	httpc := deps.HTTPClient
	if httpc == nil {
		httpc = http.DefaultClient
	}
	getFactory := deps.GetFactory
	if getFactory == nil {
		getFactory = adapter.Get
	}

	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		if rc.Envelope == nil || rc.ModelService == nil {
			abort(c, 500, domain.ErrUnknown, "internal: M3/M5 did not run before M7")
			return
		}

		// 1. 选 endpoint
		ep, err := deps.Endpoints.PickForModel(rc.Ctx, rc.Identity.TenantID, rc.ModelService.Model, rc.Identity.Group)
		if err != nil {
			abort(c, 503, domain.ErrTransient, err.Error())
			return
		}
		rc.Endpoint = ep

		// 2. 取 adapter Factory
		factory := getFactory(ep.Vendor)
		if factory == nil {
			abort(c, 500, domain.ErrUnknown, "no adapter registered for vendor: "+ep.Vendor)
			return
		}

		// 3. 构造 Session
		sess, err := factory.NewSession(rc.Ctx, ep, rc.Envelope)
		if err != nil {
			abort(c, 502, domain.ErrTransient, "adapter: NewSession failed: "+err.Error())
			return
		}
		defer func() { _ = sess.Close() }()

		// 4. 构造上游 HTTP request
		req, err := sess.BuildRequest()
		if err != nil {
			abort(c, 502, domain.ErrTransient, "adapter: BuildRequest failed: "+err.Error())
			return
		}
		req = req.WithContext(rc.Ctx)

		// 5. 调上游
		resp, err := httpc.Do(req)
		if err != nil {
			abort(c, 502, domain.ErrTransient, "upstream call failed: "+err.Error())
			return
		}
		defer func() { _ = resp.Body.Close() }()

		// 6. 转发 status + headers
		// gin 的 WriteHeader 是 lazy 的（要等 Write 才真正 send）；用 WriteHeaderNow
		// 强制立刻提交状态。这样 Feed 中途出错也不会被 M9 覆盖（流式架构固有约束：
		// 状态码一旦给客户端就不能改）。
		copyHeaders(c.Writer.Header(), resp.Header)
		c.Writer.WriteHeader(resp.StatusCode)
		c.Writer.WriteHeaderNow()

		// 7. 流式 chunk → Session.Feed → 写客户端
		buf := make([]byte, 4096)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				out, ferr := sess.Feed(buf[:n])
				if ferr != nil {
					rc.Error = &domain.AdapterError{
						Class:   domain.ErrTransient,
						Message: "adapter: Feed failed: " + ferr.Error(),
						Cause:   ferr,
					}
					return
				}
				if len(out) > 0 {
					_, _ = c.Writer.Write(out)
					c.Writer.Flush()
				}
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				rc.Error = &domain.AdapterError{
					Class:   domain.ErrTransient,
					Message: "upstream read failed: " + rerr.Error(),
					Cause:   rerr,
				}
				return
			}
		}

		// 8. Finalize
		result := sess.Finalize()
		rc.Usage = result.Usage
		if result.Error != nil {
			rc.Error = result.Error
		}
		// result.Response 在跨协议场景才用；v0.1 OpenAI-only 直接透传 chunk，不消费此字段
	}
}

// copyHeaders 把上游响应头拷贝到客户端响应头（除内部使用的 Content-Length，gin 会重算）。
func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if k == "Content-Length" {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
