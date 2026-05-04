// Package ratelimit 实现 M6 RateLimit middleware 的 Redis 限流后端。
//
// **算法**：sliding window counter（前+当前窗口加权）——消除 fixed window 边界
// 2× burst 问题；Cloudflare / Kong 业界标准。
//
// **多 key 原子**：ReserveBatch 在一次 Redis Lua call 里检查 + 消耗 N 个 bucket，
// all-or-nothing。tenant + apikey + per-model + default 多个桶可一起原子检查，
// 避免"tenant 通了但 apikey 失败时 tenant 已扣"的不一致。
//
// **TPM 两阶段**（Reserve → Adjust）：
//   - M6 ReserveBatch 时 cost = estimated_input + estimated_output（粗估）
//   - M10 真实 usage 出来后 AdjustBatch(delta)：delta>0 补扣，delta<0 退款
//
// **Headers**：Snapshot 读 bucket 当前状态，M6 / M10 写 X-RateLimit-* 响应头。
//
// 唯一实现是 Redis（pkg/ratelimit/redis.go）；删了 MemoryStore——多实例 gateway
// 必须 Redis 共享计数器，本地内存兜底等于失去限流意义。
//
// 详见 docs/architecture/04-rate-limiting.md。
package ratelimit

import (
	"context"
	"time"
)

// Bucket 描述一个限流桶的检查参数。
//
// **Cost 语义**：
//   - RPM/RPS：固定 1（每请求 +1）
//   - TPM：估算 token 数（M6 reserve）或差值（M10 adjust）
//
// **Key 命名约定**：`rl:<scope>:<subject>:<model_or_*>:<dim>`
//   - scope:    user | endpoint | model | global
//   - subject:  tenant_pin / api_key_id / endpoint_id
//   - model:    实际 model 名 或 "*"（default 跨模型桶）
//   - dim:      rpm | tpm | rps
//
// 例：
//   - rl:user:tenant:default:gpt-4o:rpm   tenant default 在 gpt-4o 上的 RPM 桶
//   - rl:user:tenant:default:*:rpm        tenant default 跨模型 default RPM 桶
//   - rl:user:apikey:ak_alice_xxx:*:tpm   apikey 跨模型 default TPM 桶
//   - rl:endpoint:42:rpm                  endpoint id=42 的 RPM 桶
type Bucket struct {
	Key    string        // 见上方命名约定
	Limit  uint32        // 窗口配额上限
	Cost   uint32        // 本次扣 N
	Window time.Duration // 窗口大小（RPM=1min, RPS=1s, TPM=1min）
}

// BucketState 单 bucket 当前状态，用于 X-RateLimit-* response headers。
type BucketState struct {
	Used      uint32
	Limit     uint32
	Remaining uint32 // = Limit - Used (clamped at 0)
	ResetAt   time.Time
}

// BucketAdjust AdjustBatch 入参：单 bucket 的调账。
//
// **Delta**：可正可负（int32，避免 uint 减法负值溢出问题）。
//   - delta>0  补扣（M10 实际 token 比 reserve 多）
//   - delta<0  退款（M10 实际 token 比 reserve 少；底层 floor at 0）
type BucketAdjust struct {
	Key    string
	Delta  int32
	Window time.Duration
}

// BucketViolation ReserveBatch 拒绝时返回的违规明细。
type BucketViolation struct {
	Key        string
	Limit      uint32
	Current    uint32
	RetryAfter time.Duration
}

// Store 限流后端。所有方法线程安全。
//
// **协议**：
//   - ReserveBatch 是原子的：要么所有 bucket 都过 + 扣，要么全不动
//   - AdjustBatch 是 best-effort：调账失败也不影响请求（M10 调用，请求已完成）
//   - Snapshot 是 read-only，可并发任意调
type Store interface {
	// ReserveBatch 多 key 原子检查 + 消耗。
	//
	// 返回 (true, nil, nil)：全部 bucket 都有余量，已 +Cost
	// 返回 (false, *BucketViolation, nil)：某个 bucket 超限，未动任何 bucket
	// 返回 (_, _, err)：Redis 错误
	ReserveBatch(ctx context.Context, buckets []Bucket) (allowed bool, violated *BucketViolation, err error)

	// AdjustBatch 调账（M10 commit TPM 真实值用）。可正可负。
	AdjustBatch(ctx context.Context, adjustments []BucketAdjust) error

	// Snapshot 读单 bucket 当前状态。给 X-RateLimit-* headers 用。
	Snapshot(ctx context.Context, b Bucket) (BucketState, error)
}
