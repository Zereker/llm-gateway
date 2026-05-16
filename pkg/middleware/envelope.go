package middleware

import (
	"errors"
	"io"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// WithSourceProtocol 把客户端协议钉在路由注册期。
//
// 路由本身告诉下游"这条路径=哪个协议"，Envelope 不再需要做 path 启发式匹配，
// 改名 / 加路径 / 多协议复用同一前缀都不会再因 Detector 误判出问题。
//
// 调用顺序：必须排在 Envelope 之前。本 middleware 在 RequestContext 上预创建
// 一个 RequestEnvelope shell（只填 SourceProtocol / Modality），Envelope 中间件
// 之后填 RawBytes / Parsed / RequestTime。
//
// proto == ProtoUnknown 视为非法（路由注册期就该明确）；不做兜底。
func WithSourceProtocol(proto domain.Protocol, mod domain.Modality) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		if rc.Envelope == nil {
			rc.Envelope = &domain.RequestEnvelope{}
		}
		rc.Envelope.SourceProtocol = proto
		rc.Envelope.Modality = mod
		c.Next()
	}
}

// Envelope 是 M3：读 body → 提取 model → 写 rc.Envelope。
//
// **职责严格收窄**：
//   - 读 body 一次，存进 rc.Envelope.RawBytes 给下游共用
//   - 从 body 顶层提 `model` 字段
//   - 写 rc.Envelope.{RawBytes, Model}
//
// **不做**的事（明确职责边界）：
//   - 参数解析 / 字段翻译——下放给 pkg/translator/<src>_<tgt>/
//   - 校验 body 合法性——translator 在 TranslateRequest 内部失败即可
//   - 流式判断——translator 内部从 body 自己读 stream 字段
//
// **不重置 c.Request.Body**：所有 body 消费者（M8 / M6 / M7 / token estimator /
// translator）都走 rc.Envelope.RawBytes；adapter 已 slim 化只构 HTTP request from
// translator 输出，不读 c.Request.Body。0 个真消费者后 NopCloser 重置就是 noise。
//
// 失败行为（统一走 abort → M9 写出 JSON）：
//   - 路由忘挂 WithSourceProtocol → 500 / ErrUnknown
//   - 读 body 失败 → 400 / ErrInvalid / "envelope: read body: <err>"
//   - 缺 model 字段 → 400 / ErrInvalid / "envelope: ..."
func Envelope() gin.HandlerFunc {
	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		ctx, end := startSpan(rc.Ctx, "llm-gateway.envelope")
		defer end()
		rc.Ctx = ctx

		if rc.Envelope == nil || rc.Envelope.SourceProtocol == domain.ProtoUnknown {
			abort(c, 500, domain.ErrUnknown, "envelope: WithSourceProtocol middleware missing")
			return
		}

		raw, err := io.ReadAll(c.Request.Body)
		if err != nil {
			abort(c, 400, domain.ErrInvalid, "envelope: read body: "+err.Error())
			return
		}
		_ = c.Request.Body.Close()

		model, err := extractModel(raw)
		if err != nil {
			abort(c, 400, domain.ErrInvalid, "envelope: "+err.Error())
			return
		}

		rc.Envelope.RawBytes = raw
		rc.Envelope.Model = model
		c.Next()
	}
}

// extractModel 从客户端 body 顶层提 `model` 字段。
//
// 三个支持的客户端协议（OpenAI Chat / Anthropic Messages / OpenAI Responses）顶层
// 字段名都是 `model`，所以不需要按 protocol 分发。
//
// **用 gjson**（不是 encoding/json）：schema-less 提取单字段，跳过整个 messages /
// tools 数组的 unmarshal——典型 4KB chat body 上 ~5x 快、1 alloc（vs stdlib 3 alloc）。
// stdlib `json.Unmarshal` 是 schema-based + 完整 tokenize；这个场景下纯浪费。
func extractModel(raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("empty body")
	}
	res := gjson.GetBytes(raw, "model")
	if !res.Exists() {
		return "", errors.New("missing 'model' field")
	}
	model := res.String()
	if model == "" {
		return "", errors.New("'model' field is empty")
	}
	return model, nil
}
