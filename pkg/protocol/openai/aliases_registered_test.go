package openai

import (
	"testing"

	"github.com/zereker/llm-gateway/pkg/protocol"
)

// 所有 OpenAI-compatible vendor 别名都注册进 registry（LookupFactory 可解析）。
func TestAliasesAllRegistered(t *testing.T) {
	for _, v := range []string{
		"ark", "deepseek", "moonshot", "zhipu", "qwen", "doubao", "minimax",
		"siliconflow", "stepfun", "groq", "together", "fireworks", "openrouter",
		"perplexity", "deepinfra", "xai", "mistral", "vllm", "ollama", "lmstudio",
	} {
		if protocol.LookupFactory(v) == nil {
			t.Errorf("vendor %q not registered", v)
		}
	}
}
