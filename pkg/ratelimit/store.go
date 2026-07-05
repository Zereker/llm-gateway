// Package ratelimit implements the rate-limiting backend for M6 / M7.
//
// **Interface** (docs/architecture/04-rate-limiting.md §5):
//
//	ReserveBatch  : pre-charge, atomic all-or-nothing across multiple keys; used for RPM/RPS
//	ChargeBatch   : post-charge, writes real usage; used for TPM; never rejects even over the limit
//	                (write succeeds + over-limit flag is set)
//	SnapshotBatch : read-only, batch-reads multiple bucket states; used by the endpoint quota
//	                read-only filter + observability
//
// **Algorithm**: sliding window counter (previous + current window weighted); the Cloudflare / Kong
// industry standard.
//
// **TPM post-charge semantics** (docs/04 §7):
//   - No estimation, no reading max_tokens, no pre-charge
//   - M6's post-side uses ChargeBatch to write real usage
//   - Even if the write pushes usage over the limit, this response has already completed and is not
//     rejected; the tpm_overflow_total metric is recorded instead
//
// **No X-RateLimit headers returned** (docs/04 §9): with multiple stacked buckets + TPM post-charge,
// an accurate "remaining" value cannot be computed.
//
// **Redis is the only implementation**: MemoryStore was removed — multi-instance gateways must share
// the counters.
package ratelimit

import (
	"context"
	"time"
)

// Bucket describes the check parameters for a single rate-limit bucket.
//
// **Cost semantics**:
//   - RPM/RPS: fixed at 1 (+1 per request)
//   - TPM: the real token count when used with ChargeBatch; should not be used with ReserveBatch
//
// **Key naming convention** (docs/04 §6):
//   - user-facing: rl:quota:<layer>:<subject>:<scope>:<dim>
//   - endpoint:    rl:endpoint:<endpoint_id>:<dim>
type Bucket struct {
	Key    string
	Limit  uint32
	Cost   uint32
	Window time.Duration
}

// BucketState is the current state of a single bucket (used by SnapshotBatch / observability).
type BucketState struct {
	Key       string
	Used      uint32
	Limit     uint32
	Remaining uint32 // = Limit - Used (clamped at 0)
	ResetAt   time.Time
}

// BucketViolation is the violation detail returned when ReserveBatch rejects a request.
type BucketViolation struct {
	Key        string
	Limit      uint32
	Current    uint32
	RetryAfter time.Duration
}

// BucketChargeResult is a single result entry from ChargeBatch.
//
// **Overflow=true** means the write pushed usage over Limit (capacity was exceeded); the caller uses
// this to record the `llm_gateway_tpm_overflow_total` metric for observability. It does not affect
// this response.
type BucketChargeResult struct {
	Key      string
	Used     uint32 // cumulative total after the write
	Limit    uint32
	Overflow bool // Used > Limit
}

// Store is the rate-limiting backend. All methods are thread-safe.
//
// **Contract**:
//   - ReserveBatch  atomic all-or-nothing; if any bucket is over the limit, none are modified
//   - ChargeBatch   best-effort write; always succeeds (unless Redis fails), over-limit only sets Overflow
//   - SnapshotBatch read-only, no side effects
type Store interface {
	// ReserveBatch atomically checks + decrements multiple keys.
	//
	// Returns (true, nil, nil): all passed and +Cost has been applied
	// Returns (false, *BucketViolation, nil): some bucket was over the limit, no bucket was modified
	// Returns (_, _, err): a Redis error occurred
	ReserveBatch(ctx context.Context, buckets []Bucket) (allowed bool, violated *BucketViolation, err error)

	// ChargeBatch post-charges: writes real usage; does not reject on over-limit, just flags Overflow
	// for the caller to report as a metric.
	//
	// The returned []BucketChargeResult is index-aligned with the input buckets.
	ChargeBatch(ctx context.Context, buckets []Bucket) ([]BucketChargeResult, error)

	// SnapshotBatch batch-reads the current state of multiple buckets; does not modify any bucket.
	SnapshotBatch(ctx context.Context, buckets []Bucket) ([]BucketState, error)
}
