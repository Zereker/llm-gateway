package openai

// init registers a set of OpenAI-compatible vendor aliases.
//
// These vendors all run the OpenAI protocol (the same /v1/chat/completions,
// the same request/response format, and Bearer token auth) — the only
// differences are endpoint URL / API key / actual model name. They can
// share Factory{} without duplicating any implementation.
//
// **Protocol ownership**: when a deployer writes endpoint SQL, they
// explicitly set `protocol: openai`; DefaultLookup picks the translator by
// ep.Protocol, decoupled from vendor.
//
// When a vendor needs vendor-specific handling (e.g. the reasoning_content
// field in DeepSeek-R1 responses, or a completely different protocol like
// Anthropic's), pull it out of this list into its own sub-package.
// Aliases returns the vendor names served by the OpenAI-compatible factory.
func Aliases() []string {
	// All OpenAI-compatible (/v1/chat/completions + Bearer). When a deployer
	// writes an endpoint, they set vendor to one of these names, protocol to
	// openai, routing.url to the respective vendor's endpoint, and auth to
	// bearer. Pull one out into its own sub-package only if it later needs
	// vendor-specific handling (e.g. DeepSeek-R1's reasoning_content).
	return []string{
		// Chinese vendors
		"ark",         // Volcano Engine Ark (ByteDance) — hosts DeepSeek / GLM / Qwen
		"deepseek",    // DeepSeek official
		"moonshot",    // Moonshot AI Kimi
		"zhipu",       // Zhipu GLM (open.bigmodel.cn's OpenAI-compat endpoint)
		"qwen",        // Alibaba DashScope compatible-mode
		"doubao",      // Doubao (Volcano Engine, named separately for billing distinction)
		"minimax",     // MiniMax
		"siliconflow", // SiliconFlow (aggregates multiple models)
		"stepfun",     // StepFun
		// Overseas aggregators / inference platforms
		"groq",       // Groq LPU inference
		"together",   // Together AI
		"fireworks",  // Fireworks AI
		"openrouter", // OpenRouter aggregator
		"perplexity", // Perplexity (sonar series)
		"deepinfra",  // DeepInfra
		"xai",        // xAI Grok (OpenAI-compat endpoint)
		"mistral",    // Mistral La Plateforme (OpenAI-compat)
		// Self-hosted
		"vllm",     // vLLM OpenAI server
		"ollama",   // Ollama /v1
		"lmstudio", // LM Studio local server
	}
}
