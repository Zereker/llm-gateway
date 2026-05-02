package middleware

import (
	"bytes"
	"io"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// Detector 识别请求的协议族与模态。M3 Envelope middleware 的依赖。
//
// 默认实现按 URL 路径优先匹配（如 /v1/messages → Anthropic + Chat），body 特征兜底。
//
// Implementations MUST be safe for concurrent use。
// body 参数：实现不可保留 slice 引用（caller 把 body 存进 RequestEnvelope.RawBytes 后会继续使用）。
type Detector interface {
	Detect(path string, body []byte) (domain.Protocol, domain.Modality)
}

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
type EnvelopeDeps struct {
	Detector Detector
	Parser   Parser
}

// Envelope 是 M3：读 body → 识别协议 / 模态 → 解析 CanonicalRequest → 写 rc.Envelope。
//
// 失败行为（统一走 abort → M9 写出 JSON）：
//   - 读 body 失败 → 400 / ErrInvalid / "read body failed: <err>"
//   - 协议未识别 → 400 / ErrInvalid / "unknown source protocol"
//   - Parser 失败 → 400 / ErrInvalid / "parse body failed: <err>"
//
// 成功后：
//   - rc.Envelope.RawBytes 持有原始字节；c.Request.Body 被替换成 NopCloser，
//     保证下游若再读 c.Request.Body 也能拿到同样的字节
//   - rc.Envelope.Parsed 持有 CanonicalRequest
//   - rc.Envelope.SourceProtocol / Modality / RequestTime 已就绪
func Envelope(deps EnvelopeDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc := GetRequestContext(c)

		raw, err := io.ReadAll(c.Request.Body)
		if err != nil {
			abort(c, 400, domain.ErrInvalid, "read body failed: "+err.Error())
			return
		}
		_ = c.Request.Body.Close()
		// 替换 body：如果上游 / Adapter 想再读 c.Request.Body，能拿到一样的内容。
		c.Request.Body = io.NopCloser(bytes.NewReader(raw))

		proto, mod := deps.Detector.Detect(c.Request.URL.Path, raw)
		if proto == domain.ProtoUnknown {
			abort(c, 400, domain.ErrInvalid, "unknown source protocol for path: "+c.Request.URL.Path)
			return
		}

		parsed, err := deps.Parser.Parse(raw, proto, mod)
		if err != nil {
			abort(c, 400, domain.ErrInvalid, "parse body failed: "+err.Error())
			return
		}

		rc.Envelope = &domain.RequestEnvelope{
			RawBytes:       raw,
			Parsed:         parsed,
			SourceProtocol: proto,
			Modality:       mod,
			RequestTime:    time.Now(),
		}
		c.Next()
	}
}
