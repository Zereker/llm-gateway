package identity

import (
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/translator"
	"github.com/zereker/llm-gateway/pkg/usage/extractor"
)

// responsesTranslator OpenAI Responses ↔ Responses identity 翻译。
//
// **背景**：Responses API 是 OpenAI 2024 H2 推出的新协议（/v1/responses），shape 跟
// Chat Completions 不同：
//
//	{
//	  "model": "gpt-4o",
//	  "input": "...",                  // 字符串 or message 数组
//	  "instructions": "...",           // system prompt
//	  "previous_response_id": "...",   // 有状态延续
//	  "stream": true,
//	  "store": true                    // 是否存
//	}
//
// **identity 用途**：客户端用 Responses SDK，上游也是 OpenAI Responses 端点的纯透传。
// 当前 OpenAI adapter 按 ep.Routing.URL 走，admin 把 endpoint 的 url 配成
// `https://api.openai.com/v1/responses` 就能用本 translator 路由。
//
// **request 端**：透传（Responses 没 stream_options 这种增强字段，不像 Chat Completions
// 需要注入 include_usage —— Responses 流式天然就在最后发 usage event）。
//
// **response 端**：handler 透传 chunk + 走 OpenAI extractor 提取 usage。
// Responses SSE event 结构跟 Chat Completions 不同，但 usage 字段名是 OpenAI-style
// （prompt_tokens / completion_tokens / total_tokens），所以 OpenAI extractor 同样适用。
//
// **跨协议**（responses_chat / responses_anthropic 等）：v1.0 minimum 不做；
// 大多数走 Responses 协议的客户端都直连 OpenAI 上游。
type responsesTranslator struct{}

// ResponsesTranslator (Responses → Responses) identity translator 公共构造器。
func ResponsesTranslator() translator.Translator { return responsesTranslator{} }

func (responsesTranslator) Source() domain.Protocol { return domain.ProtoResponses }
func (responsesTranslator) Target() domain.Protocol { return domain.ProtoResponses }

func (responsesTranslator) TranslateRequest(srcBody []byte) ([]byte, error) {
	return srcBody, nil
}

func (responsesTranslator) NewResponseHandler() translator.ResponseHandler {
	return &responsesResponseHandler{ex: extractor.NewOpenAI()}
}

type responsesResponseHandler struct {
	ex extractor.Session
}

func (h *responsesResponseHandler) Feed(chunk []byte) ([]byte, error) {
	if len(chunk) == 0 {
		return nil, nil
	}
	h.ex.Feed(chunk)
	return chunk, nil
}

func (h *responsesResponseHandler) Flush() ([]byte, *domain.Usage, error) {
	return nil, h.ex.Final(), nil
}

func init() {
	translator.Register(responsesTranslator{})
}
