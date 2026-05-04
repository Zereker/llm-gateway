package ratelimit

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore Store 的唯一实现，多实例共享计数器。
//
// 三段 Lua 脚本：
//   - reserveBatchLua：sliding window counter + 多 key 原子检查 + 消耗
//   - adjustBatchLua：M10 调账（INCRBY / DECRBY 但 floor at 0）
//   - snapshotLua：读单 bucket 当前 effective count
//
// 所有 Lua 都用 Cluster-friendly 写法（KEYS[*] 显式声明，避免 redis cluster 跨 slot
// 报错；同租户的多 bucket 应该 hashtag 化保证落同一 slot——见 keyHashTag）。
type RedisStore struct {
	rdb           *redis.Client
	scriptReserve *redis.Script
	scriptAdjust  *redis.Script
	scriptSnap    *redis.Script
}

func NewRedisStore(rdb *redis.Client) *RedisStore {
	return &RedisStore{
		rdb:           rdb,
		scriptReserve: redis.NewScript(reserveBatchLua),
		scriptAdjust:  redis.NewScript(adjustBatchLua),
		scriptSnap:    redis.NewScript(snapshotLua),
	}
}

// Sliding window counter:
//
// 把时间分成 window 大小的格子；每个 bucket 在 redis 里存两个 key：
//
//	<key>:<curStart>   当前窗口起始时刻（unix sec）的计数
//	<key>:<prevStart>  上一窗口的计数
//
// effective_now = current_count + previous_count × ((window - elapsed) / window)
//
// elapsed = 当前时刻距 curStart 的秒数；prevWeight 随 elapsed 增大而线性减少，
// 模拟"上窗口的影响逐渐淡出"。
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
    local base = 1 + (i - 1) * 3 + 1  -- ARGV index of this bucket's window
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

-- 全部检查通过；mutate
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

// AdjustBatch Lua：每个 bucket 加 delta（可正可负）。
//
// delta>0: INCRBY 直接加
// delta<0: GET → max(0, cur+delta) → SET（floor at 0，避免负计数）
//
// 注意：调账只动 current 窗口；上一窗口已经在自然衰减，调账意义不大。
//
// ARGV: now, then per-bucket {window, delta}
const adjustBatchLua = `
local now = tonumber(ARGV[1])
local n = #KEYS

for i = 1, n do
    local base = 1 + (i - 1) * 2 + 1
    local window = tonumber(ARGV[base])
    local delta = tonumber(ARGV[base + 1])

    if delta ~= 0 then
        local curStart = math.floor(now / window) * window
        local key = KEYS[i] .. ':' .. curStart
        if delta > 0 then
            redis.call('INCRBY', key, delta)
        else
            local cur = tonumber(redis.call('GET', key) or '0')
            local newval = cur + delta
            if newval < 0 then newval = 0 end
            redis.call('SET', key, newval)
        end
        redis.call('EXPIRE', key, window * 2)
    end
end
return 1
`

// Snapshot Lua：读单 bucket 当前 effective + reset_at。
//
// KEYS = {key}; ARGV = {now, window}
// 返回 {effective_count, reset_at_unix_sec}
const snapshotLua = `
local now = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local curStart = math.floor(now / window) * window
local prevStart = curStart - window
local elapsed = now - curStart
local prevWeight = (window - elapsed) / window

local cur = tonumber(redis.call('GET', KEYS[1] .. ':' .. curStart) or '0')
local prev = tonumber(redis.call('GET', KEYS[1] .. ':' .. prevStart) or '0')
local effective = cur + math.floor(prev * prevWeight)
return {effective, curStart + window}
`

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

// AdjustBatch 实现 Store.AdjustBatch。
func (s *RedisStore) AdjustBatch(ctx context.Context, adjustments []BucketAdjust) error {
	if len(adjustments) == 0 {
		return nil
	}
	keys := make([]string, len(adjustments))
	args := make([]any, 0, 1+len(adjustments)*2)
	args = append(args, time.Now().Unix())
	for i, a := range adjustments {
		keys[i] = a.Key
		windowSec := int64(a.Window.Seconds())
		if windowSec <= 0 {
			windowSec = 60
		}
		args = append(args, windowSec, a.Delta)
	}
	if _, err := s.scriptAdjust.Run(ctx, s.rdb, keys, args...).Result(); err != nil {
		return fmt.Errorf("ratelimit: adjust batch: %w", err)
	}
	return nil
}

// Snapshot 实现 Store.Snapshot。
func (s *RedisStore) Snapshot(ctx context.Context, b Bucket) (BucketState, error) {
	windowSec := int64(b.Window.Seconds())
	if windowSec <= 0 {
		windowSec = 60
	}
	res, err := s.scriptSnap.Run(ctx, s.rdb, []string{b.Key}, time.Now().Unix(), windowSec).Result()
	if err != nil {
		return BucketState{}, fmt.Errorf("ratelimit: snapshot: %w", err)
	}
	arr, ok := res.([]any)
	if !ok || len(arr) != 2 {
		return BucketState{}, fmt.Errorf("ratelimit: unexpected snapshot result %v", res)
	}
	used, _ := toInt(arr[0])
	resetSec, _ := toInt(arr[1])
	st := BucketState{
		Used:    uint32(used),
		Limit:   b.Limit,
		ResetAt: time.Unix(resetSec, 0),
	}
	if b.Limit > st.Used {
		st.Remaining = b.Limit - st.Used
	}
	return st, nil
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
