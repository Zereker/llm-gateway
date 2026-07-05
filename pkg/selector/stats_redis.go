package selector

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStatsStore 是 EndpointStatsStore 的 Redis 实现——多副本 gateway 共享同一份
// per-endpoint EMA 统计,打分一致(InMemoryStatsStore 每副本各算各的,多副本下评分
// 会漂移)。接口跟 InMemory 版完全一致,cmd 按 scoring.driver 切换。
//
// **存储**:每个 endpoint 一个 hash `<prefix>:epstats:<id>`,字段
// latency_ms / success_rate / sample_count / updated。TTL 让长期无流量的 endpoint
// 统计自然过期(回到中性)。
//
// **原子 EMA**:Record 用一段 Lua EVAL 做 read-modify-write,避免多副本并发
// Record 互相覆盖。best-effort——Redis 错不阻塞调度(Report 本就是 fire-and-forget)。
type RedisStatsStore struct {
	rdb    *redis.Client
	prefix string
	decay  float64
	ttl    time.Duration
}

// NewRedisStatsStore 构造;decay<=0 用 0.2,ttl<=0 用 1h,prefix 空用 "llm-gateway:sched"。
func NewRedisStatsStore(rdb *redis.Client, prefix string, decay float64, ttl time.Duration) *RedisStatsStore {
	if decay <= 0 || decay > 1 {
		decay = 0.2
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	if prefix == "" {
		prefix = "llm-gateway:sched"
	}
	return &RedisStatsStore{rdb: rdb, prefix: prefix, decay: decay, ttl: ttl}
}

func (s *RedisStatsStore) key(endpointID int64) string {
	return s.prefix + ":epstats:" + strconv.FormatInt(endpointID, 10)
}

// recordScript 原子 EMA:有历史则加权更新,无历史则直接取本次值;末尾刷新 TTL。
// KEYS[1]=hash key; ARGV=[latency, success01, decay, now_unix, ttl_sec]。
var recordScript = redis.NewScript(`
local k = KEYS[1]
local lat = tonumber(ARGV[1])
local suc = tonumber(ARGV[2])
local decay = tonumber(ARGV[3])
local cur = redis.call('HMGET', k, 'latency_ms', 'success_rate', 'sample_count')
local nlat, nsuc, ncnt
if cur[3] then
  nlat = decay*lat + (1-decay)*tonumber(cur[1])
  nsuc = decay*suc + (1-decay)*tonumber(cur[2])
  ncnt = tonumber(cur[3]) + 1
else
  nlat = lat
  nsuc = suc
  ncnt = 1
end
redis.call('HSET', k, 'latency_ms', nlat, 'success_rate', nsuc, 'sample_count', ncnt, 'updated', ARGV[4])
redis.call('EXPIRE', k, tonumber(ARGV[5]))
return ncnt
`)

// Record EMA 更新单 endpoint 的 latency / success(原子)。best-effort。
func (s *RedisStatsStore) Record(ctx context.Context, endpointID int64, result Result) {
	if endpointID == 0 {
		return
	}
	_ = recordScript.Run(ctx, s.rdb, []string{s.key(endpointID)},
		float64(result.Latency.Milliseconds()),
		success01(result.Class),
		s.decay,
		time.Now().Unix(),
		int(s.ttl.Seconds()),
	).Err()
}

// Snapshot 取单 endpoint 当前快照;无数据 / Redis 错都返回中性快照
// (SuccessRate=1, SampleCount=0)——跟 InMemory 版语义一致,DefaultScorer 会给中性
// factor 保留探索流量。
func (s *RedisStatsStore) Snapshot(ctx context.Context, endpointID int64) EndpointStats {
	neutral := EndpointStats{SuccessRate: 1.0}
	if endpointID == 0 {
		return neutral
	}
	vals, err := s.rdb.HMGet(ctx, s.key(endpointID),
		"latency_ms", "success_rate", "sample_count", "updated").Result()
	if err != nil || len(vals) != 4 || vals[2] == nil {
		return neutral
	}
	cnt := parseUint32(vals[2])
	if cnt == 0 {
		return neutral
	}
	return EndpointStats{
		LatencyMs:   parseFloat(vals[0]),
		SuccessRate: parseFloat(vals[1]),
		SampleCount: cnt,
		Updated:     time.Unix(parseInt64(vals[3]), 0),
	}
}

func parseFloat(v any) float64 {
	s, ok := v.(string)
	if !ok {
		return 0
	}
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func parseUint32(v any) uint32 {
	s, ok := v.(string)
	if !ok {
		return 0
	}
	n, _ := strconv.ParseFloat(s, 64) // Lua 存的是 number,可能带小数,先按 float 再截
	if n < 0 {
		return 0
	}
	return uint32(n)
}

func parseInt64(v any) int64 {
	s, ok := v.(string)
	if !ok {
		return 0
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// 编译期断言。
var _ EndpointStatsStore = (*RedisStatsStore)(nil)
