package ratelimit

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore is the only implementation of Store; counters are shared across multiple instances
// (docs/04 §5 §7).
//
// Three Lua scripts:
//   - reserveBatchLua  : sliding window counter + atomic multi-key check + charge (all-or-nothing)
//   - chargeBatchLua   : post-charge writes the real cost; returns an Overflow flag when over Limit
//   - snapshotBatchLua : batch-reads the current effective value + reset_at for multiple buckets; read-only
type RedisStore struct {
	rdb            *redis.Client
	scriptReserve  *redis.Script
	scriptCharge   *redis.Script
	scriptSnapshot *redis.Script
}

func NewRedisStore(rdb *redis.Client) *RedisStore {
	return &RedisStore{
		rdb:            rdb,
		scriptReserve:  redis.NewScript(reserveBatchLua),
		scriptCharge:   redis.NewScript(chargeBatchLua),
		scriptSnapshot: redis.NewScript(snapshotBatchLua),
	}
}

// =============================================================================
// Lua scripts
// =============================================================================

// Sliding window counter:
//
// Time is divided into window-sized slots; each bucket stores two keys in redis:
//
//	<key>:<curStart>   the count for the current window's start time (unix sec)
//	<key>:<prevStart>  the count for the previous window
//
// effective_now = current_count + previous_count × ((window - elapsed) / window)
//
// **Atomic across multiple keys**: phase 1 checks all buckets first; only if all pass does phase 2
// run INCRBY.
//
// Returns the array {allowed, violated_index, violated_current, violated_limit, retry_after_seconds}
//
//	{1, 0, 0, 0, 0}      = all allowed
//	{0, i, cur, lim, ra} = the i-th bucket was rejected (i starts at 1; Lua style)
//
// ARGV layout: now, then per-bucket {window, limit, cost}
const reserveBatchLua = `
local now = tonumber(ARGV[1])
local n = #KEYS
local checks = {}

for i = 1, n do
    local base = 1 + (i - 1) * 3 + 1
    local window = tonumber(ARGV[base])
    local limit = tonumber(ARGV[base + 1])
    local cost = tonumber(ARGV[base + 2])

    local curStart = math.floor(now / window) * window
    local prevStart = curStart - window
    local elapsed = now - curStart
    local prevWeight = (window - elapsed) / window

    local cur = tonumber(redis.call('GET', KEYS[i] .. ':' .. curStart) or '0')
    local prev = tonumber(redis.call('GET', KEYS[i] .. ':' .. prevStart) or '0')
    local effective = cur + math.floor(prev * prevWeight)

    if effective + cost > limit then
        local retry = math.ceil(window - elapsed)
        return {0, i, effective, limit, retry}
    end
    checks[i] = curStart
end

for i = 1, n do
    local base = 1 + (i - 1) * 3 + 1
    local window = tonumber(ARGV[base])
    local cost = tonumber(ARGV[base + 2])
    local curStart = checks[i]
    local key = KEYS[i] .. ':' .. curStart
    redis.call('INCRBY', key, cost)
    redis.call('EXPIRE', key, window * 2)
end
return {1, 0, 0, 0, 0}
`

// ChargeBatch Lua: writes cost for each bucket; never rejects, but returns an Overflow flag.
//
// **Difference from ReserveBatch** (docs/04 §5 §7):
//   - reserve is atomic all-or-nothing; charge always writes (unless Redis fails)
//   - when reserve rejects, this request fails; when charge writes over the limit, it only flags
//     Overflow for the metric
//
// **Does not read the prev window**: charge is after-the-fact accounting and doesn't need sliding
// validation — it just runs INCRBY on the current window directly. The effective value with prev
// decay is computed later when read via SnapshotBatch / observability.
//
// ARGV: now, then per-bucket {window, limit, cost}
// Returns {used_after, limit, overflow} × N (flattened)
const chargeBatchLua = `
local now = tonumber(ARGV[1])
local n = #KEYS
local out = {}

for i = 1, n do
    local base = 1 + (i - 1) * 3 + 1
    local window = tonumber(ARGV[base])
    local limit = tonumber(ARGV[base + 1])
    local cost = tonumber(ARGV[base + 2])

    local curStart = math.floor(now / window) * window
    local key = KEYS[i] .. ':' .. curStart
    local used = 0
    if cost > 0 then
        used = redis.call('INCRBY', key, cost)
        redis.call('EXPIRE', key, window * 2)
    else
        used = tonumber(redis.call('GET', key) or '0')
    end
    local overflow = 0
    if used > limit then overflow = 1 end
    out[(i - 1) * 3 + 1] = used
    out[(i - 1) * 3 + 2] = limit
    out[(i - 1) * 3 + 3] = overflow
end
return out
`

// SnapshotBatch Lua: batch-reads bucket effective + reset_at; read-only, no writes.
//
// ARGV: now, then per-bucket {window, limit}
// Returns {used, limit, reset_at} × N (flattened)
const snapshotBatchLua = `
local now = tonumber(ARGV[1])
local n = #KEYS
local out = {}

for i = 1, n do
    local base = 1 + (i - 1) * 2 + 1
    local window = tonumber(ARGV[base])
    local limit = tonumber(ARGV[base + 1])

    local curStart = math.floor(now / window) * window
    local prevStart = curStart - window
    local elapsed = now - curStart
    local prevWeight = (window - elapsed) / window

    local cur = tonumber(redis.call('GET', KEYS[i] .. ':' .. curStart) or '0')
    local prev = tonumber(redis.call('GET', KEYS[i] .. ':' .. prevStart) or '0')
    local effective = cur + math.floor(prev * prevWeight)

    out[(i - 1) * 3 + 1] = effective
    out[(i - 1) * 3 + 2] = limit
    out[(i - 1) * 3 + 3] = curStart + window
end
return out
`

// =============================================================================
// Store impl
// =============================================================================

// ReserveBatch implements Store.ReserveBatch.
func (s *RedisStore) ReserveBatch(ctx context.Context, buckets []Bucket) (bool, *BucketViolation, error) {
	if len(buckets) == 0 {
		return true, nil, nil
	}
	keys := make([]string, len(buckets))
	args := make([]any, 0, 1+len(buckets)*3)
	args = append(args, time.Now().Unix())
	for i, b := range buckets {
		keys[i] = b.Key
		windowSec := int64(b.Window.Seconds())
		if windowSec <= 0 {
			windowSec = 60
		}
		args = append(args, windowSec, b.Limit, b.Cost)
	}

	res, err := s.scriptReserve.Run(ctx, s.rdb, keys, args...).Result()
	if err != nil {
		return false, nil, fmt.Errorf("ratelimit: reserve batch: %w", err)
	}
	arr, ok := res.([]any)
	if !ok || len(arr) != 5 {
		return false, nil, fmt.Errorf("ratelimit: unexpected reserve result %T %v", res, res)
	}
	allowed, _ := toInt(arr[0])
	if allowed == 1 {
		return true, nil, nil
	}
	idx, _ := toInt(arr[1])
	cur, _ := toInt(arr[2])
	lim, _ := toInt(arr[3])
	retry, _ := toInt(arr[4])
	if idx < 1 || int(idx) > len(buckets) {
		return false, nil, fmt.Errorf("ratelimit: invalid violated index %d", idx)
	}
	violated := &BucketViolation{
		Key:        buckets[idx-1].Key,
		Limit:      uint32(lim),
		Current:    uint32(cur),
		RetryAfter: time.Duration(retry) * time.Second,
	}
	return false, violated, nil
}

// ChargeBatch implements Store.ChargeBatch (docs/04 §5 §7).
func (s *RedisStore) ChargeBatch(ctx context.Context, buckets []Bucket) ([]BucketChargeResult, error) {
	if len(buckets) == 0 {
		return nil, nil
	}
	keys := make([]string, len(buckets))
	args := make([]any, 0, 1+len(buckets)*3)
	args = append(args, time.Now().Unix())
	for i, b := range buckets {
		keys[i] = b.Key
		windowSec := int64(b.Window.Seconds())
		if windowSec <= 0 {
			windowSec = 60
		}
		args = append(args, windowSec, b.Limit, b.Cost)
	}

	res, err := s.scriptCharge.Run(ctx, s.rdb, keys, args...).Result()
	if err != nil {
		return nil, fmt.Errorf("ratelimit: charge batch: %w", err)
	}
	arr, ok := res.([]any)
	if !ok || len(arr) != len(buckets)*3 {
		return nil, fmt.Errorf("ratelimit: unexpected charge result len=%d want=%d", len(arr), len(buckets)*3)
	}
	out := make([]BucketChargeResult, len(buckets))
	for i := range buckets {
		used, _ := toInt(arr[i*3])
		lim, _ := toInt(arr[i*3+1])
		overflow, _ := toInt(arr[i*3+2])
		out[i] = BucketChargeResult{
			Key:      buckets[i].Key,
			Used:     uint32(used),
			Limit:    uint32(lim),
			Overflow: overflow == 1,
		}
	}
	return out, nil
}

// SnapshotBatch implements Store.SnapshotBatch.
func (s *RedisStore) SnapshotBatch(ctx context.Context, buckets []Bucket) ([]BucketState, error) {
	if len(buckets) == 0 {
		return nil, nil
	}
	keys := make([]string, len(buckets))
	args := make([]any, 0, 1+len(buckets)*2)
	args = append(args, time.Now().Unix())
	for i, b := range buckets {
		keys[i] = b.Key
		windowSec := int64(b.Window.Seconds())
		if windowSec <= 0 {
			windowSec = 60
		}
		args = append(args, windowSec, b.Limit)
	}

	res, err := s.scriptSnapshot.Run(ctx, s.rdb, keys, args...).Result()
	if err != nil {
		return nil, fmt.Errorf("ratelimit: snapshot batch: %w", err)
	}
	arr, ok := res.([]any)
	if !ok || len(arr) != len(buckets)*3 {
		return nil, fmt.Errorf("ratelimit: unexpected snapshot result len=%d want=%d", len(arr), len(buckets)*3)
	}
	out := make([]BucketState, len(buckets))
	for i := range buckets {
		used, _ := toInt(arr[i*3])
		lim, _ := toInt(arr[i*3+1])
		reset, _ := toInt(arr[i*3+2])
		st := BucketState{
			Key:     buckets[i].Key,
			Used:    uint32(used),
			Limit:   uint32(lim),
			ResetAt: time.Unix(reset, 0),
		}
		if st.Limit > st.Used {
			st.Remaining = st.Limit - st.Used
		}
		out[i] = st
	}
	return out, nil
}

// toInt parses the number returned by Lua (go-redis returns integers as int64, but watch out for string).
func toInt(v any) (int64, error) {
	switch x := v.(type) {
	case int64:
		return x, nil
	case int:
		return int64(x), nil
	case string:
		return strconv.ParseInt(x, 10, 64)
	default:
		return 0, fmt.Errorf("toInt: unsupported %T", v)
	}
}

// Compile-time assertion.
var _ Store = (*RedisStore)(nil)
