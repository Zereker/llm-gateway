package middleware

import "testing"

func TestEstimateTokens_EmptyBody(t *testing.T) {
	got := EstimateTokens(nil, 100)
	if got != 100 {
		t.Errorf("nil body: got=%d, want=100 (input=0 + fallback)", got)
	}
}

func TestEstimateTokens_InputCharsDiv4PlusMaxTokens(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"hello"}],"max_tokens":256}`)
	// input = len/4 = 80/4 = 20
	got := EstimateTokens(body, 4096)
	// max_tokens 解析成功 → cost = 20 + 256 = 276
	if got != uint32(len(body)/4)+256 {
		t.Errorf("got=%d, want=%d", got, uint32(len(body)/4)+256)
	}
}

func TestEstimateTokens_MaxTokensMissing_UsesFallback(t *testing.T) {
	body := []byte(`{"model":"x","messages":[]}`)
	got := EstimateTokens(body, 4096)
	want := uint32(len(body)/4) + 4096
	if got != want {
		t.Errorf("got=%d, want=%d", got, want)
	}
}

func TestEstimateTokens_MalformedJSON_UsesFallback(t *testing.T) {
	body := []byte(`not json at all`)
	got := EstimateTokens(body, 100)
	// input = len/4 = 15/4 = 3
	// max_tokens 解析失败 → fallback=100
	if got != uint32(len(body)/4)+100 {
		t.Errorf("got=%d, want=%d", got, uint32(len(body)/4)+100)
	}
}

func TestEstimateTokens_MaxTokensZero_UsesFallback(t *testing.T) {
	body := []byte(`{"max_tokens":0}`)
	got := EstimateTokens(body, 999)
	want := uint32(len(body)/4) + 999
	if got != want {
		t.Errorf("got=%d, want=%d", got, want)
	}
}

func TestEstimateTokens_TinyBody_InputAtLeast1(t *testing.T) {
	// len=3 → /4=0；estimateInputTokens 返回 1 兜底
	body := []byte(`abc`)
	got := EstimateTokens(body, 0)
	// input=1（floor），fallback=0 → max_tokens=0
	if got != 1 {
		t.Errorf("got=%d, want=1 (1 floor input + 0 max_tokens)", got)
	}
}
