package schedule

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// CooldownManager endpoint 失败冷却的存储抽象。
//
// **Mark**：endpoint 失败时调；按 ErrorClass 决定 TTL。后续 InCooldown 在 TTL 内返 true。
// **InCooldown**：filter 用，批量查多个 endpoint 是否在冷却。
//
// **多实例一致性**：Redis 后端共享状态，所有 gateway 实例看到同一份 cooldown view。
type CooldownManager interface {
	Mark(ctx context.Context, endpointID int64, class ErrorClass) error
	InCooldown(ctx context.Context, endpointIDs []int64) (map[int64]bool, error)
}

// CooldownDurations 各 ErrorClass 对应的冷却时长（来自 cfg.Scheduler.Cooldown）。
type CooldownDurations struct {
	Transient time.Duration
	Capacity  time.Duration
	Permanent time.Duration
	Invalid   time.Duration
	Unknown   time.Duration
}

// Get 按 class 取对应时长；class 不映射或时长 0 时返回 0（不冷却）。
func (d CooldownDurations) Get(class ErrorClass) time.Duration {
	switch class {
	case ClassTransient:
		return d.Transient
	case ClassCapacity:
		return d.Capacity
	case ClassPermanent:
		return d.Permanent
	case ClassInvalid:
		return d.Invalid
	case ClassUnknown:
		return d.Unknown
	default:
		return 0
	}
}

// =============================================================================
// Redis 实现
// =============================================================================

// RedisCooldownManager Redis 后端的 CooldownManager。
//
// 存储约定：
//
//	key:   cd:endpoint:<id>
//	value: ErrorClass 字符串（"transient" / "capacity" / ...）；用于诊断
//	TTL:   按 ErrorClass 配置；过期自动清理（不需主动 GC）
//
// **Mark 实现**：直接 SET key value EX ttl —— 后到的 Mark 会覆盖前一次的 TTL。
// 这意味着如果一个 endpoint 反复失败，TTL 一直被刷新延长——符合"持续失败保持冷却"的语义。
type RedisCooldownManager struct {
	rdb       *redis.Client
	durations CooldownDurations
}

// NewRedisCooldownManager 构造一个 Redis 后端的 manager。
func NewRedisCooldownManager(rdb *redis.Client, d CooldownDurations) *RedisCooldownManager {
	return &RedisCooldownManager{rdb: rdb, durations: d}
}

// Mark 标 endpoint 进入冷却；TTL 按 class 决定。
func (m *RedisCooldownManager) Mark(ctx context.Context, endpointID int64, class ErrorClass) error {
	if endpointID == 0 {
		return nil
	}
	ttl := m.durations.Get(class)
	if ttl <= 0 {
		// 该 class 配的 0 或负值 = 不冷却
		return nil
	}
	key := cooldownKey(endpointID)
	if err := m.rdb.Set(ctx, key, class.String(), ttl).Err(); err != nil {
		return fmt.Errorf("cooldown: set %s: %w", key, err)
	}
	return nil
}

// InCooldown 批量查多个 endpoint 是否在冷却（CooldownFilter 用）。
//
// 用 MGET 一次拿所有 key 的值；存在 key 就视为冷却中（值是 ErrorClass 字符串，目前只用做诊断）。
func (m *RedisCooldownManager) InCooldown(ctx context.Context, endpointIDs []int64) (map[int64]bool, error) {
	if len(endpointIDs) == 0 {
		return nil, nil
	}
	keys := make([]string, len(endpointIDs))
	for i, id := range endpointIDs {
		keys[i] = cooldownKey(id)
	}
	vals, err := m.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("cooldown: mget: %w", err)
	}
	out := make(map[int64]bool, len(endpointIDs))
	for i, v := range vals {
		if v != nil {
			out[endpointIDs[i]] = true
		}
	}
	return out, nil
}

func cooldownKey(endpointID int64) string {
	return "cd:endpoint:" + strconv.FormatInt(endpointID, 10)
}

// 编译期断言。
var _ CooldownManager = (*RedisCooldownManager)(nil)

// =============================================================================
// CooldownFilter
// =============================================================================

// CooldownFilter 排除冷却中候选；Filter 链的第一道屏障（最便宜的过滤，先做）。
//
// **Redis 错误处理**：Redis 挂了 InCooldown 返 err；filter 这里 fail-open
// （所有候选都通过）—— 优于全部拒（导致 503 风暴）。
type CooldownFilter struct {
	mgr CooldownManager
}

// NewCooldownFilter 构造一个 cooldown filter。
func NewCooldownFilter(mgr CooldownManager) *CooldownFilter {
	return &CooldownFilter{mgr: mgr}
}

func (f *CooldownFilter) Name() string { return "cooldown" }

func (f *CooldownFilter) Apply(ctx context.Context, candidates []*domain.Endpoint, _ *Request) []*domain.Endpoint {
	if len(candidates) == 0 || f.mgr == nil {
		return candidates
	}
	ids := make([]int64, len(candidates))
	for i, ep := range candidates {
		ids[i] = ep.ID
	}
	cooled, err := f.mgr.InCooldown(ctx, ids)
	if err != nil {
		// fail-open：Redis 错时不要把所有 endpoint 都过滤掉
		return candidates
	}
	out := make([]*domain.Endpoint, 0, len(candidates))
	for _, ep := range candidates {
		if !cooled[ep.ID] {
			out = append(out, ep)
		}
	}
	return out
}

// 编译期断言。
var _ Filter = (*CooldownFilter)(nil)
