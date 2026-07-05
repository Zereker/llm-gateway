package selector

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStatsStore is a Redis implementation of EndpointStatsStore — multiple gateway replicas share
// the same per-endpoint EMA stats, keeping scoring consistent (InMemoryStatsStore computes independently
// per replica, so scoring drifts across replicas). The interface is fully identical to the InMemory
// version; cmd switches between them via scoring.driver.
//
// **Storage**: one hash per endpoint, `<prefix>:epstats:<id>`, with fields
// latency_ms / success_rate / sample_count / updated. TTL lets stats for long-idle endpoints
// naturally expire (returning to neutral).
//
// **Atomic EMA**: Record uses a Lua EVAL script to do the read-modify-write, avoiding concurrent
// Records from different replicas overwriting each other. Best-effort — a Redis error doesn't block
// scheduling (Report is inherently fire-and-forget).
type RedisStatsStore struct {
	rdb    *redis.Client
	prefix string
	decay  float64
	ttl    time.Duration
}

// NewRedisStatsStore constructs one; decay<=0 uses 0.2, ttl<=0 uses 1h, empty prefix uses "llm-gateway:sched".
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

// recordScript performs an atomic EMA: weighted update if history exists, otherwise take this
// value directly; refreshes TTL at the end.
// KEYS[1]=hash key; ARGV=[latency, success01, decay, now_unix, ttl_sec].
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

// Record updates a single endpoint's latency / success via EMA (atomic). Best-effort.
func (s *RedisStatsStore) Record(ctx context.Context, endpointID int64, result Result) {
	if endpointID == 0 {
		return
	}
	// Clamp to >=1s: a sub-second TTL gets truncated to 0 by int(), and EXPIRE key 0 deletes
	// the key immediately — every Record would be discarded right after being written, leaving
	// Snapshot permanently neutral and silently disabling scoring.
	ttlSec := int64(s.ttl.Seconds())
	if ttlSec < 1 {
		ttlSec = 1
	}
	_ = recordScript.Run(ctx, s.rdb, []string{s.key(endpointID)},
		float64(result.Latency.Milliseconds()),
		success01(result.Class),
		s.decay,
		time.Now().Unix(),
		ttlSec,
	).Err()
}

// Snapshot takes the current snapshot for a single endpoint; both no-data and Redis errors return
// a neutral snapshot (SuccessRate=1, SampleCount=0) — matching the InMemory version's semantics,
// so DefaultScorer gives a neutral factor and preserves exploration traffic.
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
	n, _ := strconv.ParseFloat(s, 64) // Lua stores a number, which may have decimals; parse as float first, then truncate
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

// Compile-time assertion.
var _ EndpointStatsStore = (*RedisStatsStore)(nil)
