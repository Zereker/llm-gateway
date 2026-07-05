package openai

import "github.com/zereker/llm-gateway/pkg/protocol"

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
	// 都是 OpenAI-compatible（/v1/chat/completions + Bearer）。deployer 写 endpoint 时
	// vendor 填这些名之一、protocol 填 openai、routing.url 指各家端点、auth 填 bearer。
	// 出现专属处理需求（DeepSeek-R1 的 reasoning_content 等）再抽独立子包。
	aliases := []string{
		// 中国厂商
		"ark",         // 火山方舟（字节）—— DeepSeek / GLM / Qwen 托管
		"deepseek",    // DeepSeek 官方
		"moonshot",    // 月之暗面 Kimi
		"zhipu",       // 智谱 GLM（open.bigmodel.cn 的 OpenAI-compat 端点）
		"qwen",        // 阿里 DashScope compatible-mode
		"doubao",      // 豆包（火山，独立命名便于区分计费）
		"minimax",     // MiniMax
		"siliconflow", // 硅基流动（聚合多模型）
		"stepfun",     // 阶跃星辰
		// 海外聚合 / 推理平台
		"groq",       // Groq LPU 推理
		"together",   // Together AI
		"fireworks",  // Fireworks AI
		"openrouter", // OpenRouter 聚合
		"perplexity", // Perplexity（sonar 系列）
		"deepinfra",  // DeepInfra
		"xai",        // xAI Grok（OpenAI-compat 端点）
		"mistral",    // Mistral La Plateforme（OpenAI-compat）
		// 自托管
		"vllm",     // vLLM OpenAI server
		"ollama",   // Ollama /v1
		"lmstudio", // LM Studio 本地服务
	}
	for _, v := range aliases {
		protocol.RegisterFactory(v, Factory{})
	}
}
