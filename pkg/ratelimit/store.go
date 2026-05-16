// Package ratelimit 实现 M6 / M7 的限流后端。
//
// **接口**（docs/architecture/04-rate-limiting.md §5）：
//
//	ReserveBatch  : 前扣，多 key 原子 all-or-nothing；RPM/RPS 使用
//	ChargeBatch   : 后扣，按真实 usage 写入；TPM 使用；即使超上限也不拒绝（写入成功 + over-limit 标记）
//	SnapshotBatch : 只读，批量读多个 bucket 状态；endpoint quota read-only filter + 观测使用
//
// **算法**：sliding window counter（前 + 当前窗口加权）；Cloudflare / Kong 业界标准。
//
// **TPM 后扣语义**（docs/04 §7）：
//   - 不预估、不读 max_tokens、不预扣
//   - M6 post-side 用 ChargeBatch 写真实 usage
//   - 即使写入后超过上限，本次响应已完成不再拒；记 tpm_overflow_total metric
//
// **不返回 X-RateLimit headers**（docs/04 §9）：多桶叠加 + TPM 后扣无法准确给出 remaining。
//
// **Redis 唯一实现**：删了 MemoryStore——多实例 gateway 必须共享计数器。
package ratelimit

import (
	"context"
	"time"
)

// Bucket 描述一个限流桶的检查参数。
//
// **Cost 语义**：
//   - RPM/RPS：固定 1（每请求 +1）
//   - TPM：ChargeBatch 时为真实 token 数；ReserveBatch 时不应使用
//
// **Key 命名约定**（docs/04 §6）：
//   - 用户侧 ：rl:quota:<layer>:<subject>:<scope>:<dim>
//   - endpoint：rl:endpoint:<endpoint_id>:<dim>
type Bucket struct {
	Key    string
	Limit  uint32
	Cost   uint32
	Window time.Duration
}

// BucketState 单 bucket 当前状态（SnapshotBatch / observability 用）。
type BucketState struct {
	Key       string
	Used      uint32
	Limit     uint32
	Remaining uint32 // = Limit - Used (clamped at 0)
	ResetAt   time.Time
}

// BucketViolation ReserveBatch 拒绝时返回的违规明细。
type BucketViolation struct {
	Key        string
	Limit      uint32
	Current    uint32
	RetryAfter time.Duration
}

// BucketChargeResult ChargeBatch 单条结果。
//
// **Overflow=true** 表示写入后超过 Limit（容量超载），调用方据此记
// `llm_gateway_tpm_overflow_total` metric 用于观测；不影响本次响应。
type BucketChargeResult struct {
	Key      string
	Used     uint32 // 写入后的累计
	Limit    uint32
	Overflow bool // Used > Limit
}

// Store 限流后端。所有方法线程安全。
//
// **协议**：
//   - ReserveBatch  原子 all-or-nothing；任一 bucket 超限则全不动
//   - ChargeBatch   best-effort 写入；总会成功（除非 Redis 故障），超限只标 Overflow
//   - SnapshotBatch 只读，无副作用
type Store interface {
	// ReserveBatch 多 key 原子 check + 扣减。
	//
	// 返回 (true, nil, nil)：全过且已 +Cost
	// 返回 (false, *BucketViolation, nil)：某 bucket 超限，未动任何 bucket
	// 返回 (_, _, err)：Redis 错误
	ReserveBatch(ctx context.Context, buckets []Bucket) (allowed bool, violated *BucketViolation, err error)

	// ChargeBatch 后扣：写真实用量；超限不拒，标 Overflow 给调用方上报 metric。
	//
	// 返回的 []BucketChargeResult 索引对齐入参 buckets。
	ChargeBatch(ctx context.Context, buckets []Bucket) ([]BucketChargeResult, error)

	// SnapshotBatch 批量读多 bucket 当前状态；不修改任何 bucket。
	SnapshotBatch(ctx context.Context, buckets []Bucket) ([]BucketState, error)
}
