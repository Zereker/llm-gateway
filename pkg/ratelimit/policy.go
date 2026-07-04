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

// PolicyRule 是 quota_policies.rule_json 解析后的 typed shape：
//
//	{
//	  "default":   {"rpm": 60, "tpm": 100000, "rps": null, "concurrent_requests": null},
//	  "per_model": {"gpt-4o": {"rpm": 10, "tpm": 30000}}
//	}
//
// 选 rule 策略（M6 用）：**additive**——
//   - 如果 default 存在 → 消耗 default bucket
//   - 如果 per_model[currentModel] 存在 → **同时**消耗 per-model bucket
//   - 两者一起 reserve 在同一个原子 ReserveBatch 调用里
//   - 这样 per-model 是 default 的 sub-cap（OpenAI tier 语义）
//
// 跟之前互斥语义的区别：
//   - **互斥**（之前）：per-model 命中就只用 per-model；总量可能超 default 上限
//   - **additive**（现在）：per-model 永远 ≤ default；总量受 default 严格约束
type PolicyRule struct {
	Default  *repo.QuotaConfig           `json:"default,omitempty"`
	PerModel map[string]repo.QuotaConfig `json:"per_model,omitempty"`
}

// CachedPolicy LRU/TTL 缓存的预解析 policy。
//
// Rule 是完整解析后的形态（json.Unmarshal 已跑过）；M6 拿到直接用，不重复解析。
type CachedPolicy struct {
	ID   int64
	Rule *PolicyRule
}

// PolicyCache QuotaPolicyProvider 的缓存层。
//
// **设计**：sync.Map + 每条目带过期时间（lazy 清理）。
// policy 数量预期几十量级，不需要 LRU；TTL 控制最长 staleness。
//
// **TTL 默认 30s**：SQL 改 policy 后窗口期内 gateway 仍走旧值——可接受
// （限流不需要严格实时；大改时手动 Invalidate 或重启）。
type PolicyCache struct {
	upstream repo.QuotaPolicyProvider
	ttl      time.Duration
	entries  sync.Map // int64 → *cacheEntry
}

type cacheEntry struct {
	rule    *PolicyRule // nil 表示 upstream 也没找到（policy 不存在）
	expires time.Time
}

// NewPolicyCache 包一层 upstream，TTL 默认 30s。
func NewPolicyCache(upstream repo.QuotaPolicyProvider, ttl time.Duration) *PolicyCache {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &PolicyCache{upstream: upstream, ttl: ttl}
}

// Get 命中 cache 直接返回；miss / 过期则查 upstream + 解析 rule_json + 缓存。
//
// 返回 (nil, nil) 表示该 policy ID 在 upstream 也不存在（视作"该层不限"）。
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
		// 过期：fallthrough 重查
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
		// **悬空 policy id**：account/api_key 挂了 quota_policy_id 但表里没有这行
		// （typo / 被 hard-delete）。语义上按"该层不限"处理（NULL = 不限的既有
		// 契约），但这**多半是配置事故**——静默放行等于悄悄关掉限流，只能在账单
		// 上发现。warn + metric 让它可见（30s cache 让 warn 频率有界）。
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

// Invalidate 显式清缓存（SQL 改 policy 后调一次让窗口期内立即生效；可选）。
func (c *PolicyCache) Invalidate(id int64) {
	c.entries.Delete(id)
}

// PickRulesAdditive 按 additive 语义选出本次请求要消耗的 (default, per_model) 两个 rule。
//
// 返回的 *QuotaConfig 列表里：
//   - 第一个永远是 default（如果存在）
//   - 第二个是 per_model[currentModel]（如果存在）
//   - 两者都不存在 → 返回空切片（该层不限）
//
// scope 标识 bucket 维度：
//   - default rule → scope = "*"（跨模型聚合桶）
//   - per_model rule → scope = currentModel（per-model 独立桶）
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
			qCopy := q // 复制避免取 map value 地址
			out = append(out, ScopedRule{Scope: model, Quota: &qCopy})
		}
	}
	return out
}

// ScopedRule "某个 scope 上的限流配置"。M6 据此构造 Bucket。
type ScopedRule struct {
	Scope string            // "*" (default) 或 实际 model 名
	Quota *repo.QuotaConfig // RPM/TPM/RPS/ConcurrentRequests 任填
}

// 编译期：确保签名稳定。
var _ = errors.New
