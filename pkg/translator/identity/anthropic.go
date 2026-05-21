package identity

import (
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/translator"
	"github.com/zereker/llm-gateway/pkg/usage/extractor"
)

// anthropicTranslator Anthropic ↔ Anthropic identity 翻译。
//
// 客户端用 Anthropic SDK 发请求 → 上游也是 Anthropic（包括真 anthropic.com 或
// Bedrock 转发的 anthropic-compatible 端点）的场景。
//
// **request 端**：透传（Anthropic 没有 stream_options 这种增强字段，无需注入）。
// **response 端**：handler 透传 chunk + 走 extractor 旁路提取 usage（v0.5 G6 抽出）。
type anthropicTranslator struct{}

// AnthropicTranslator (Anthropic → Anthropic) identity translator 公共构造器。
func AnthropicTranslator() translator.Translator { return anthropicTranslator{} }

func (anthropicTranslator) Source() domain.Protocol { return domain.ProtoAnthropic }
func (anthropicTranslator) Target() domain.Protocol { return domain.ProtoAnthropic }

func (anthropicTranslator) TranslateRequest(srcBody []byte) ([]byte, error) {
	return srcBody, nil
}

func (anthropicTranslator) NewResponseHandler() translator.ResponseHandler {
	return &anthropicResponseHandler{ex: extractor.NewAnthropic()}
}

type anthropicResponseHandler struct {
	ex extractor.Session
}

func (h *anthropicResponseHandler) Feed(chunk []byte) ([]byte, error) {
	if len(chunk) == 0 {
		return nil, nil
	}
	h.ex.Feed(chunk)
	// 同协议透传：原样返回 chunk 给客户端
	return chunk, nil
}

func (h *anthropicResponseHandler) Flush() ([]byte, *domain.Usage, error) {
	return nil, h.ex.Final(), nil
}

func init() {
	translator.Register(anthropicTranslator{})
}
