package selector

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// AffinityStore 会话亲和（sticky routing）的 session → endpoint 映射存储。
//
// 用途:同一会话固定打到同一个上游 endpoint,提升 vLLM prefix cache / KV cache
// 命中率(endpoint 的 PrefixCacheEnabled 能力位是这个场景的信号)。
//
// **软亲和**:只是"更倾向",不是硬绑定——pinned endpoint 被 cooldown / 排除 /
// 下线时,scheduler 自动重选并重新 pin(见 scheduler.Pick)。多副本要一致,所以走
// Redis;nil = 不开亲和。
type AffinityStore interface {
	// Get 取 session 当前 pin 的 endpoint id;无映射返回 (0,false)。
	Get(ctx context.Context, sessionKey string) (int64, bool)
	// Set 记录/刷新 session → endpoint 映射(每次选中都刷 TTL,活跃会话保持粘住)。
	Set(ctx context.Context, sessionKey string, endpointID int64)
}

// RedisAffinityStore Redis 实现,多副本共享。
type RedisAffinityStore struct {
	rdb    *redis.Client
	prefix string
	ttl    time.Duration
}

// NewRedisAffinityStore 构造;prefix 空用 "llm-gateway:sched",ttl<=0 用 10m。
func NewRedisAffinityStore(rdb *redis.Client, prefix string, ttl time.Duration) *RedisAffinityStore {
	if prefix == "" {
		prefix = "llm-gateway:sched"
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &RedisAffinityStore{rdb: rdb, prefix: prefix, ttl: ttl}
}

func (s *RedisAffinityStore) key(sessionKey string) string {
	return s.prefix + ":affinity:" + sessionKey
}

// Get 读 pin;缺失 / Redis 错都当无映射(best-effort,不阻塞调度)。
func (s *RedisAffinityStore) Get(ctx context.Context, sessionKey string) (int64, bool) {
	v, err := s.rdb.Get(ctx, s.key(sessionKey)).Result()
	if err != nil {
		return 0, false
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id == 0 {
		return 0, false
	}
	return id, true
}

// Set 写 pin + 刷 TTL(best-effort)。
func (s *RedisAffinityStore) Set(ctx context.Context, sessionKey string, endpointID int64) {
	if endpointID == 0 {
		return
	}
	_ = s.rdb.Set(ctx, s.key(sessionKey), endpointID, s.ttl).Err()
}

// 编译期断言。
var _ AffinityStore = (*RedisAffinityStore)(nil)
