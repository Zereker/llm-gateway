package openai
import ("testing"; "github.com/zereker/llm-gateway/pkg/protocol")
func TestAliasesAllRegistered(t *testing.T) {
	for _, v := range []string{"deepseek","moonshot","zhipu","qwen","groq","together","fireworks","openrouter","perplexity","xai","mistral","vllm","ollama","siliconflow","doubao","minimax","stepfun","deepinfra","lmstudio","ark"} {
		if protocol.LookupFactory(v) == nil { t.Errorf("vendor %q not registered", v) }
	}
}
