package ratelimit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/zereker/llm-gateway/pkg/metric"
	"github.com/zereker/llm-gateway/pkg/repo"
)

// PolicyRule is the typed shape after parsing quota_policies.rule_json:
//
//	{
//	  "default":   {"rpm": 60, "tpm": 100000, "rps": null, "concurrent_requests": null},
//	  "per_model": {"gpt-4o": {"rpm": 10, "tpm": 30000}}
//	}
//
// Rule selection strategy (used by M6): **additive** —
//   - if default exists → charge the default bucket
//   - if per_model[currentModel] exists → **also** charge the per-model bucket
//   - both are reserved together in the same atomic ReserveBatch call
//   - this makes per-model a sub-cap of default (OpenAI tier semantics)
//
// Difference from the previous mutually-exclusive semantics:
//   - **mutually exclusive** (before): a per-model match meant only per-model was used; total usage
//     could exceed the default limit
//   - **additive** (now): per-model is always ≤ default; total usage is strictly bounded by default
type PolicyRule struct {
	Default  *repo.QuotaConfig           `json:"default,omitempty"`
	PerModel map[string]repo.QuotaConfig `json:"per_model,omitempty"`
}

// CachedPolicy is a pre-parsed policy cached with LRU/TTL semantics.
//
// Rule is the fully parsed form (json.Unmarshal has already run); M6 uses it directly without
// re-parsing.
type CachedPolicy struct {
	ID   int64
	Rule *PolicyRule
}

// PolicyCache is the cache layer over QuotaPolicyProvider.
//
// **Design**: sync.Map + a per-entry expiration time (lazy eviction).
// The number of policies is expected to be in the dozens, so an LRU isn't needed; TTL bounds the
// maximum staleness.
//
// **Default TTL 30s**: after a policy is changed via SQL, the gateway keeps using the old value
// during the window — this is acceptable (rate limiting doesn't need strict real-time consistency;
// call Invalidate manually or restart for larger changes).
type PolicyCache struct {
	upstream repo.QuotaPolicyProvider
	ttl      time.Duration
	entries  sync.Map // int64 → *cacheEntry
}

type cacheEntry struct {
	rule    *PolicyRule // nil means upstream didn't find it either (policy doesn't exist)
	expires time.Time
}

// NewPolicyCache wraps upstream with a cache; TTL defaults to 30s.
func NewPolicyCache(upstream repo.QuotaPolicyProvider, ttl time.Duration) *PolicyCache {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &PolicyCache{upstream: upstream, ttl: ttl}
}

// Get returns directly on a cache hit; on miss / expiry it queries upstream, parses rule_json, and
// caches the result.
//
// Returns (nil, nil) if the policy ID isn't found in upstream either (treated as "this layer is
// unlimited").
func (c *PolicyCache) Get(ctx context.Context, id int64) (*PolicyRule, error) {
	if id == 0 {
		return nil, nil
	}
	now := time.Now()
	if v, ok := c.entries.Load(id); ok {
		e := v.(*cacheEntry)
		if now.Before(e.expires) {
			return e.rule, nil
		}
		// expired: fall through and re-query
	}

	pol, err := c.upstream.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("policy_cache: upstream: %w", err)
	}
	var rule *PolicyRule
	if pol != nil && len(pol.RuleJSON) > 0 {
		var r PolicyRule
		if err := json.Unmarshal(pol.RuleJSON, &r); err != nil {
			return nil, fmt.Errorf("policy_cache: parse rule_json id=%d: %w", id, err)
		}
		rule = &r
	}
	if rule == nil {
		// **Dangling policy id**: an account/api_key references a quota_policy_id but the row
		// doesn't exist in the table (typo / hard-deleted). Semantically this is treated as
		// "this layer is unlimited" (the existing NULL = unlimited contract), but this is
		// **most likely a configuration accident** — silently allowing it would quietly disable
		// rate limiting, only discoverable via the bill. warn + metric make it visible (the 30s
		// cache bounds the warn frequency).
		slog.WarnContext(ctx, "policy_cache: quota_policy_id dangling; treating as unlimited",
			"policy_id", id)
		metric.Inc(metric.PolicyCacheTotal, "layer", "any", "result", "dangling")
	}
	c.entries.Store(id, &cacheEntry{
		rule:    rule,
		expires: now.Add(c.ttl),
	})
	return rule, nil
}

// Invalidate explicitly clears the cache entry (optional; call once after changing a policy via SQL
// to make the change take effect immediately instead of waiting out the window).
func (c *PolicyCache) Invalidate(id int64) {
	c.entries.Delete(id)
}

// PickRulesAdditive selects, per additive semantics, the (default, per_model) rules to charge for
// this request.
//
// In the returned *QuotaConfig list:
//   - the first is always default (if it exists)
//   - the second is per_model[currentModel] (if it exists)
//   - if neither exists → returns an empty slice (this layer is unlimited)
//
// scope identifies the bucket dimension:
//   - default rule → scope = "*" (aggregated bucket across models)
//   - per_model rule → scope = currentModel (independent per-model bucket)
func (r *PolicyRule) PickRulesAdditive(model string) []ScopedRule {
	if r == nil {
		return nil
	}
	out := make([]ScopedRule, 0, 2)
	if r.Default != nil && !r.Default.IsEmpty() {
		out = append(out, ScopedRule{Scope: "*", Quota: r.Default})
	}
	if model != "" && r.PerModel != nil {
		if q, ok := r.PerModel[model]; ok && !q.IsEmpty() {
			qCopy := q // copy to avoid taking the address of a map value
			out = append(out, ScopedRule{Scope: model, Quota: &qCopy})
		}
	}
	return out
}

// ScopedRule is "the rate-limit config for a given scope". M6 builds a Bucket from this.
type ScopedRule struct {
	Scope string            // "*" (default) or the actual model name
	Quota *repo.QuotaConfig // any of RPM/TPM/RPS/ConcurrentRequests may be set
}

// Compile-time: ensure the signature stays stable.
var _ = errors.New
