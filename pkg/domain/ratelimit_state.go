package domain

// RateLimitState M6 RateLimit middleware 的产物，传给 M10 做 TPM 调账。
//
// **TPM 两阶段**：
//   - M6 reserve 估算 cost（input chars/4 + max_tokens）；记录 TPMBucketKeys + ReservedTPM
//   - M10 拿真实 rc.Usage.Total - ReservedTPM 算 delta；非零时 store.AdjustBatch(TPMBucketKeys, delta)
//
// **TightestBucketKey + TightestLimit + TightestUsed + TightestResetAtUnix**：
// M6 reserve 成功后挑最紧的 bucket（按 Limit 升序）snapshot，写到这；M10 在 dump
// usage event 时附加为 X-RateLimit-* 反查依据。
//
// 没有引用 ratelimit.Bucket 是为了避免 domain 反向依赖 pkg/ratelimit。
type RateLimitState struct {
	// TPM 两阶段调账数据
	ReservedTPM   uint32   // M6 估的总 token cost
	TPMBucketKeys []string // 这些 bucket 在 M10 commit 时一起 AdjustBatch

	// Tightest bucket（用于 X-RateLimit-* headers / debug）
	TightestBucketKey  string
	TightestLimit      uint32
	TightestUsed       uint32
	TightestResetAtSec int64
	TightestWindowSec  int64
}
