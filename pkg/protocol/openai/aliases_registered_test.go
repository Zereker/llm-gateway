package openai

import (
	"testing"

	"github.com/zereker/llm-gateway/pkg/protocol"
)

// TestAliasesAllRegistered verifies all OpenAI-compatible vendor aliases are
// registered into the registry (resolvable via LookupFactory).
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
