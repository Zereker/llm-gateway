package middleware

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// DefaultParser 把请求 body 反序列化为 CanonicalRequest——**只 unmarshal 路由必需字段**。
//
// **审计结论（v0.5）**：M5 只用 rc.Envelope.Parsed.Model；流式判断在 translator 内自己读
// raw body，不需要 Parsed.Stream；Messages / Tools / Metadata 等没人用。所以这里只 probe
// `model` 一个字段，跳过大字段——长聊天历史 body 几十 KB 时省下大量 alloc + GC。
//
// **如果将来 Parsed 需要更多字段**：扩 modelProbe；不要回退到全量 Unmarshal CanonicalRequest。
//
// **支持的协议**：ProtoOpenAI / ProtoAnthropic / ProtoResponses（三协议顶层都有 model 字段）。
//
// 完整的 body shape 翻译在 pkg/translator 各 translator 内做；body 全文走 RawBytes 透传。
//
// 零值即可用：var p DefaultParser。
type DefaultParser struct{}

// modelProbe 只取 model 字段；不导出避免外部错误依赖完整解析。
type modelProbe struct {
	Model string `json:"model"`
}

// Parse 实现 middleware.Parser.Parse。
func (DefaultParser) Parse(raw []byte, proto domain.Protocol, _ domain.Modality) (domain.CanonicalRequest, error) {
	if len(raw) == 0 {
		return domain.CanonicalRequest{}, errors.New("default parser: empty body")
	}
	switch proto {
	case domain.ProtoOpenAI, domain.ProtoAnthropic, domain.ProtoResponses:
		// 三协议顶层都有 "model" 字段；本 probe 通用
	default:
		return domain.CanonicalRequest{}, fmt.Errorf("default parser: unsupported protocol %s (handles openai/anthropic/responses)", proto)
	}
	var p modelProbe
	if err := json.Unmarshal(raw, &p); err != nil {
		return domain.CanonicalRequest{}, fmt.Errorf("default parser: %w", err)
	}
	if p.Model == "" {
		return domain.CanonicalRequest{}, errors.New("default parser: missing 'model' field")
	}
	return domain.CanonicalRequest{Model: p.Model}, nil
}

// 编译期断言。
var _ Parser = DefaultParser{}
