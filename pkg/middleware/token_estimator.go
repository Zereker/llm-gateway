package middleware

import (
	"encoding/json"
)

// TokenEstimator M6 RateLimit 给 TPM 桶 reserve 用的"粗估"工具。
//
// 真实 token 数要等响应才知道（M10 commit 时调账）；M6 这里只是 pre-check：
//   - input：按 prompt 字符数 / 4 估（OpenAI tokenizer 平均字英比）
//   - output：取请求体里的 max_tokens；没指定取 default 4096
//
// 估算偏保守（往大了估），M10 调账后退款。这样：
//   - 不会因为低估导致并发超 TPM
//   - 不会因为高估太多导致正常请求被错杀（取舍：定 default 4096 是大多数场景安全）

// EstimateTokens 拿请求 raw body + 默认 max_tokens 估总 token 数（input + output）。
//
// 用法（在 M6 middleware 里）：
//
//	cost := middleware.EstimateTokens(rc.Envelope.RawBytes, 4096)
//	bucket := ratelimit.Bucket{..., Cost: cost, ...}
func EstimateTokens(rawBody []byte, defaultMaxOutput uint32) uint32 {
	input := estimateInputTokens(rawBody)
	output := extractMaxTokens(rawBody, defaultMaxOutput)
	return input + output
}

// estimateInputTokens 按 chars/4 粗估 prompt token 数。
//
// 不解析 JSON 结构；直接拿整个 body 字节数 / 4。会把 system / messages /
// tool_definitions 等所有内容都算进去（保守估）。中文字符一个汉字 ≈ 2 token，
// 这种粗估会偏低；M10 真实计费 + 调账修正。
func estimateInputTokens(rawBody []byte) uint32 {
	if len(rawBody) == 0 {
		return 0
	}
	tokens := uint32(len(rawBody) / 4)
	if tokens == 0 {
		return 1
	}
	return tokens
}

// extractMaxTokens 从 OpenAI-compatible body 取 max_tokens 字段。
//
// OpenAI 协议：`max_tokens` (整型，默认 16/inf 取决于 model)；
// Anthropic 协议：`max_tokens` (整型，必填)；
// 其它协议：可能是 `max_output_tokens` 等——v0.5 第一刀只看 max_tokens。
//
// 解析失败 / 字段缺失 / 0 → 用 fallback。
func extractMaxTokens(rawBody []byte, fallback uint32) uint32 {
	if len(rawBody) == 0 {
		return fallback
	}
	// 用 json.RawMessage 避免完整 unmarshal——只取 max_tokens 一个字段
	var probe struct {
		MaxTokens *uint32 `json:"max_tokens"`
	}
	if err := json.Unmarshal(rawBody, &probe); err != nil {
		return fallback
	}
	if probe.MaxTokens == nil || *probe.MaxTokens == 0 {
		return fallback
	}
	return *probe.MaxTokens
}
