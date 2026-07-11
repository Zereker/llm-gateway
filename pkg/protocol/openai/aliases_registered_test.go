package openai

import "testing"

func TestAliasesComplete(t *testing.T) {
	want := []string{
		"ark", "deepseek", "moonshot", "zhipu", "qwen", "doubao", "minimax",
		"siliconflow", "stepfun", "groq", "together", "fireworks", "openrouter",
		"perplexity", "deepinfra", "xai", "mistral", "vllm", "ollama", "lmstudio",
	}
	got := Aliases()
	if len(got) != len(want) {
		t.Fatalf("aliases len=%d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("alias[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}
