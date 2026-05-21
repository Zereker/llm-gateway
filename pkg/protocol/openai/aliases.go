package openai

import "github.com/zereker/llm-gateway/pkg/adapter"

// init 注册一组 OpenAI-compatible 的 vendor 别名。
//
// 这些 vendor 都跑 OpenAI 协议（同样的 /v1/chat/completions、同样的请求/响应格式
// 和 Bearer token 鉴权），区别只在 endpoint URL / API key / 实际模型名。
// 共用 Factory{} 即可，不需要复制实现。
//
// **协议归属**：deployer 写 endpoint SQL 时显式填 `protocol: openai`；DefaultLookup
// 拿 endpoint 时按 ep.Protocol 取 translator，跟 vendor 解耦。
//
// 当某个 vendor 出现专属处理需求时（例如 DeepSeek-R1 响应里的 reasoning_content
// 字段、或 Anthropic 那种完全不同的协议），把它从这个列表里抽出去独立成子包。
func init() {
	aliases := []string{
		"ark", // 火山方舟（Volcengine Ark）—— 字节跳动的模型托管平台，承载 DeepSeek / GLM / Qwen 等
		// 后续可加：moonshot（月之暗面）/ zhipu（智谱）/ qwen（阿里）/ doubao（豆包）等
	}
	for _, v := range aliases {
		adapter.Register(v, Factory{})
	}
}
