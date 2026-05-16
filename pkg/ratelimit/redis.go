package ratelimit

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore Store 的唯一实现，多实例共享计数器（docs/04 §5 §7）。
//
// 三段 Lua 脚本：
//   - reserveBatchLua  : sliding window counter + 多 key 原子检查 + 消耗（all-or-nothing）
//   - chargeBatchLua   : 后扣写真实 cost；过 Limit 返 Overflow 标记
//   - snapshotBatchLua : 批量读多 bucket 当前 effective + reset_at；只读
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
// Lua 脚本
// =============================================================================

// Sliding window counter:
//
// 把时间分成 window 大小的格子；每个 bucket 在 redis 里存两个 key：
//
//	<key>:<curStart>   当前窗口起始时刻（unix sec）的计数
//	<key>:<prevStart>  上一窗口的计数
//
// effective_now = current_count + previous_count × ((window - elapsed) / window)
//
// **多 key 原子**：先 phase 1 检查所有 buckets，全过才 phase 2 INCRBY。
//
// 返回数组 {allowed, violated_index, violated_current, violated_limit, retry_after_seconds}
//
//	{1, 0, 0, 0, 0}      = 全部允许
//	{0, i, cur, lim, ra} = 第 i 个 bucket 拒绝（i 从 1 起；Lua 风格）
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

// ChargeBatch Lua：每个 bucket 写 cost；不拒绝，但返 Overflow 标记。
//
// **跟 ReserveBatch 区别**（docs/04 §5 §7）：
//   - reserve 是原子 all-or-nothing；charge 总是写入（除非 Redis 故障）
//   - reserve 拒绝时本次请求失败；charge 写入后超限只标 Overflow 给 metric
//
// **不读 prev 窗口**：charge 是事后记账，不需要 sliding 校验——直接 INCRBY 当前
// 窗口。effective 由 SnapshotBatch / observability 读出来时再算 prev 衰减。
//
// ARGV: now, then per-bucket {window, limit, cost}
// 返回 {used_after, limit, overflow} × N （平铺）
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

// SnapshotBatch Lua：批量读 bucket effective + reset_at；只读不写。
//
// ARGV: now, then per-bucket {window, limit}
// 返回 {used, limit, reset_at} × N （平铺）
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

// ReserveBatch 实现 Store.ReserveBatch。
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

// ChargeBatch 实现 Store.ChargeBatch（docs/04 §5 §7）。
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

// SnapshotBatch 实现 Store.SnapshotBatch。
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

// toInt 把 Lua 返回的数字解出来（go-redis 把整数返回成 int64，但小心 string）。
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

// 编译期断言。
var _ Store = (*RedisStore)(nil)
