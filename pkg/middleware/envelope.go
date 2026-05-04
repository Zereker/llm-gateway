package middleware

import (
	"bytes"
	"io"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// Parser 把 RawBytes 解析为 CanonicalRequest。M3 Envelope middleware 的依赖。
//
// 不同 SourceProtocol 用不同实现；Parser 内部按 SourceProtocol 分发。
//
// Implementations MUST be safe for concurrent use。
// raw 参数：实现不可保留 slice 引用；解析后的 CanonicalRequest 应是独立的 Go 对象。
type Parser interface {
	Parse(raw []byte, proto domain.Protocol, mod domain.Modality) (domain.CanonicalRequest, error)
}

// EnvelopeDeps M3 Envelope middleware 的依赖。
//
// 没有 Detector：协议 / 模态由前置 WithSourceProtocol middleware 在路由注册期写入；
// 本中间件只负责 body / Parsed / RequestTime。
type EnvelopeDeps struct {
	Parser Parser
}

// Envelope 是 M3：读 body → Parser 解析 CanonicalRequest → 填 rc.Envelope。
//
// 协议 / 模态由前置的 WithSourceProtocol middleware（路由注册期）写入 rc.Envelope.SourceProtocol /
// Modality；Gemini 之类 model-在-URL 的协议靠后置的 WithGeminiPathModel 把 model 补进
// rc.Envelope.Parsed。
//
// 失败行为（统一走 abort → M9 写出 JSON）：
//   - 路由忘挂 WithSourceProtocol → 500 / ErrUnknown（装配错，启动期就该发现）
//   - 读 body 失败 → 400 / ErrInvalid / "read body failed: <err>"
//   - Parser 失败 → 400 / ErrInvalid / "parse body failed: <err>"
//
// 成功后：
//   - rc.Envelope.RawBytes 持有原始字节；c.Request.Body 被替换成 NopCloser，
//     保证下游若再读 c.Request.Body 也能拿到同样的字节
//   - rc.Envelope.Parsed 持有 CanonicalRequest（Gemini 的 Model 由 WithGeminiPathModel 后填）
//   - rc.Envelope.RequestTime 已就绪
func Envelope(deps EnvelopeDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		if rc.Envelope == nil || rc.Envelope.SourceProtocol == domain.ProtoUnknown {
			abort(c, 500, domain.ErrUnknown, "WithSourceProtocol middleware missing before Envelope")
			return
		}

		raw, err := io.ReadAll(c.Request.Body)
		if err != nil {
			abort(c, 400, domain.ErrInvalid, "read body failed: "+err.Error())
			return
		}
		_ = c.Request.Body.Close()
		// 替换 body：如果上游 / Adapter 想再读 c.Request.Body，能拿到一样的内容。
		c.Request.Body = io.NopCloser(bytes.NewReader(raw))

		parsed, err := deps.Parser.Parse(raw, rc.Envelope.SourceProtocol, rc.Envelope.Modality)
		if err != nil {
			abort(c, 400, domain.ErrInvalid, "parse body failed: "+err.Error())
			return
		}

		rc.Envelope.RawBytes = raw
		rc.Envelope.Parsed = parsed
		rc.Envelope.RequestTime = time.Now()
		c.Next()
	}
}
