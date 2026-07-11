package selector

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zereker/llm-gateway/internal/domain"
)

// CooldownManager is the storage abstraction for endpoint failure cooldown.
//
// **Mark**: called when an endpoint fails; TTL is decided by ErrorClass. When the
// upstream told us exactly when capacity comes back (Retry-After / rate-limit
// reset headers), retryAfter carries that hint and overrides the static
// per-class TTL (clamped — see RedisCooldownManager.Mark). Pass 0 when no hint
// is available. Subsequent InCooldown returns true within the TTL.
// **InCooldown**: used by filter, batch-checks whether multiple endpoints are cooling down.
// **Clear**: removes an endpoint's cooldown early — used by the health prober
// when a probe confirms the endpoint has recovered before the TTL expired.
//
// **ClearIfRecoverable**: used by the health prober for probe-gated recovery —
// releases an endpoint's cooldown early, but ONLY when the cooled class is one
// a successful health probe can actually attest to (Transient / Capacity). A
// Permanent cooldown (401/403 bad credentials, config error) is left in place:
// a health endpoint returning 200 does not prove the API key is valid, so
// clearing it would flap the endpoint back into rotation to fail auth again.
// The compare-and-delete is atomic, so a stale probe cannot wipe a differently
// classed cooldown that was Marked after the probe started.
//
// **Multi-instance consistency**: the Redis backend shares state, so all gateway instances see the same cooldown view.
type CooldownManager interface {
	Mark(ctx context.Context, endpointID int64, class ErrorClass, retryAfter time.Duration) error
	InCooldown(ctx context.Context, endpointIDs []int64) (map[int64]bool, error)
	ClearIfRecoverable(ctx context.Context, endpointID int64) (cleared bool, err error)
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

// Reset-aware TTL clamp bounds: an upstream Retry-After / reset header
// overrides the static per-class TTL, but only within [floor, cap] —
// the floor absorbs sub-second resets (a 200ms cooldown is pure churn),
// the cap stops a pathological upstream ("retry in 24h") from poisoning
// the endpoint far longer than any static class TTL would.
const (
	resetTTLFloor = 1 * time.Second
	resetTTLCap   = 10 * time.Minute
)

// resolveCooldownTTL decides the effective cooldown TTL.
//
// retryAfter > 0 (an upstream Retry-After / rate-limit reset hint) wins,
// clamped to [resetTTLFloor, resetTTLCap]; otherwise the static per-class
// duration applies. A class configured to 0 disables cooldown entirely —
// the hint is ignored too, since the deployer opted this class out.
func resolveCooldownTTL(d CooldownDurations, class ErrorClass, retryAfter time.Duration) time.Duration {
	ttl := d.Get(class)
	if ttl <= 0 {
		return 0
	}
	if retryAfter > 0 {
		return min(max(retryAfter, resetTTLFloor), resetTTLCap)
	}
	return ttl
}

// jitterTTL spreads a TTL by ±10%. When a vendor-wide incident cools a whole
// batch of endpoints at once (all with the same static class TTL, or the
// same upstream Retry-After), identical TTLs would make them all recover at
// the same instant and re-form a synchronized retry storm; the jitter
// staggers the thundering herd.
func jitterTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return ttl
	}

	return time.Duration(float64(ttl) * (0.9 + 0.2*rand.Float64()))
}

// Mark marks an endpoint as entering cooldown; the TTL follows
// resolveCooldownTTL (reset-aware when the upstream provided a hint), with
// ±10% jitter applied on top (see jitterTTL).
func (m *RedisCooldownManager) Mark(ctx context.Context, endpointID int64, class ErrorClass, retryAfter time.Duration) error {
	if endpointID == 0 {
		return nil
	}
	ttl := jitterTTL(resolveCooldownTTL(m.durations, class, retryAfter))
	if ttl <= 0 {
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

// clearRecoverableScript atomically deletes the cooldown key only when its
// stored class is probe-recoverable (transient / capacity). Returns 1 if it
// deleted, 0 otherwise (key absent, or a non-recoverable class like permanent).
// Atomicity closes the Mark/Clear race: a stale probe can't delete a cooldown
// that was re-Marked with a different class after the probe observed health.
var clearRecoverableScript = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if v == 'transient' or v == 'capacity' then
  redis.call('DEL', KEYS[1])
  return 1
end
return 0
`)

// ClearIfRecoverable releases an endpoint's cooldown early, but only when the
// cooled class is one a health probe can attest to (see recoverableCooldownClasses).
// Returns whether a key was actually cleared. A key that doesn't exist, or one
// holding a non-recoverable class, is a no-op returning (false, nil).
func (m *RedisCooldownManager) ClearIfRecoverable(ctx context.Context, endpointID int64) (bool, error) {
	if endpointID == 0 {
		return false, nil
	}
	key := cooldownKey(endpointID)
	n, err := clearRecoverableScript.Run(ctx, m.rdb, []string{key}).Int()
	if err != nil {
		return false, fmt.Errorf("cooldown: clear %s: %w", key, err)
	}
	return n == 1, nil
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
