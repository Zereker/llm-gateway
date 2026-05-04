// Package identity 提供"同协议"translator：客户端协议 = 上游协议时用，
// request 几乎透传，response 仅做 SSE 透传 + usage 提取。
//
// init() 注册 ProtoOpenAI ↔ ProtoOpenAI（OpenAI client → OpenAI-compatible upstream，
// 涵盖真 OpenAI / DeepSeek / ARK / Qwen 等所有 OpenAI 协议族 vendor）。
//
// 后续加 Anthropic ↔ Anthropic / Gemini ↔ Gemini 等纯透传场景同样在本包扩展。
//
// **usage 提取**走 pkg/usage/extractor 的 OpenAI Session（v0.5 G6 抽出来的）；
// 本 handler 主路径只管 chunk 透传。
package identity

import (
	"encoding/json"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/translator"
	"github.com/zereker-labs/ai-gateway/pkg/usage/extractor"
)

// openaiTranslator OpenAI ↔ OpenAI identity 翻译。
//
// **request 端**：自动给 stream=true 的请求注入 stream_options.include_usage = true，
// 让上游必返回 usage chunk。
//
// **response 端**：handler 透传 chunk 给客户端 + 走 extractor 旁路提取 usage。
type openaiTranslator struct{}

func (openaiTranslator) Source() domain.Protocol { return domain.ProtoOpenAI }
func (openaiTranslator) Target() domain.Protocol { return domain.ProtoOpenAI }

func (openaiTranslator) TranslateRequest(srcBody []byte) ([]byte, error) {
	return ensureStreamUsage(srcBody), nil
}

func (openaiTranslator) NewResponseHandler() translator.ResponseHandler {
	return &openaiResponseHandler{ex: extractor.NewOpenAI()}
}

type openaiResponseHandler struct {
	ex extractor.Session
}

func (h *openaiResponseHandler) Feed(chunk []byte) ([]byte, error) {
	if len(chunk) == 0 {
		return nil, nil
	}
	h.ex.Feed(chunk)
	// 同协议透传：原样返回 chunk 给客户端
	return chunk, nil
}

func (h *openaiResponseHandler) Flush() ([]byte, *domain.Usage, error) {
	return nil, h.ex.Final(), nil
}

// ensureStreamUsage 在流式 body 中保证 stream_options.include_usage = true。
//
// 失败（JSON 不合法 / 非流式）时返回原 body —— translator 不应因这个增强失败而中止。
func ensureStreamUsage(body []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	// 非流式不动
	if streamRaw, ok := m["stream"]; ok {
		var stream bool
		if err := json.Unmarshal(streamRaw, &stream); err == nil && !stream {
			return body
		}
	} else {
		// 没 stream 字段 = 默认 false（OpenAI 行为），不注入
		return body
	}

	// 解 stream_options 子对象
	var so map[string]json.RawMessage
	if raw, ok := m["stream_options"]; ok {
		_ = json.Unmarshal(raw, &so)
	}
	if so == nil {
		so = make(map[string]json.RawMessage)
	}
	so["include_usage"] = json.RawMessage(`true`)

	soBytes, err := json.Marshal(so)
	if err != nil {
		return body
	}
	m["stream_options"] = soBytes
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func init() {
	translator.Register(openaiTranslator{})
}
