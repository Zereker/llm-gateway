package selector

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// CooldownManager is the storage abstraction for endpoint failure cooldown.
//
// **Mark**: called when an endpoint fails; TTL is decided by ErrorClass. Subsequent InCooldown returns true within the TTL.
// **InCooldown**: used by filter, batch-checks whether multiple endpoints are cooling down.
//
// **Multi-instance consistency**: the Redis backend shares state, so all gateway instances see the same cooldown view.
type CooldownManager interface {
	Mark(ctx context.Context, endpointID int64, class ErrorClass) error
	InCooldown(ctx context.Context, endpointIDs []int64) (map[int64]bool, error)
}

// CooldownDurations holds the cooldown duration for each ErrorClass (from cfg.Selector.Cooldown).
type CooldownDurations struct {
	Transient time.Duration
	Capacity  time.Duration
	Permanent time.Duration
	Invalid   time.Duration
	Unknown   time.Duration
}

// Get returns the duration for a class; returns 0 (no cooldown) if the class isn't mapped or the duration is 0.
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
// Redis implementation
// =============================================================================

// RedisCooldownManager is a Redis-backed CooldownManager.
//
// Storage convention:
//
//	key:   cd:endpoint:<id>
//	value: ErrorClass string ("transient" / "capacity" / ...); used for diagnostics
//	TTL:   configured per ErrorClass; expires automatically (no active GC needed)
//
// **Mark implementation**: directly SET key value EX ttl — a later Mark overwrites the previous TTL.
// This means that if an endpoint keeps failing, its TTL keeps getting refreshed and extended —
// matching the semantics of "keep cooling down while failures persist".
type RedisCooldownManager struct {
	rdb       *redis.Client
	durations CooldownDurations
}

// NewRedisCooldownManager constructs a Redis-backed manager.
func NewRedisCooldownManager(rdb *redis.Client, d CooldownDurations) *RedisCooldownManager {
	return &RedisCooldownManager{rdb: rdb, durations: d}
}

// Mark marks an endpoint as entering cooldown; TTL is decided by class.
func (m *RedisCooldownManager) Mark(ctx context.Context, endpointID int64, class ErrorClass) error {
	if endpointID == 0 {
		return nil
	}
	ttl := m.durations.Get(class)
	if ttl <= 0 {
		// this class is configured with 0 or negative = no cooldown
		return nil
	}
	key := cooldownKey(endpointID)
	if err := m.rdb.Set(ctx, key, class.String(), ttl).Err(); err != nil {
		return fmt.Errorf("cooldown: set %s: %w", key, err)
	}
	return nil
}

// InCooldown checks in batch whether multiple endpoints are cooling down (used by CooldownFilter).
//
// Uses MGET to fetch all key values in one call; a present key is treated as cooling down (the value is the ErrorClass string, currently only used for diagnostics).
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

// Compile-time assertion.
var _ CooldownManager = (*RedisCooldownManager)(nil)

// =============================================================================
// CooldownFilter
// =============================================================================

// CooldownFilter excludes candidates that are cooling down; the first barrier in the Filter chain (cheapest filter, done first).
//
// **Redis error handling**: if Redis is down, InCooldown returns an err; this filter fails open here
// (all candidates pass through) — better than rejecting everything (which would cause a 503 storm).
type CooldownFilter struct {
	mgr CooldownManager
}

// NewCooldownFilter constructs a cooldown filter.
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
		// fail-open: don't filter out all endpoints when Redis errors
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

// Compile-time assertion.
var _ Filter = (*CooldownFilter)(nil)
