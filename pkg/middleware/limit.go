package middleware

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/ratelimit"
	"github.com/zereker-labs/ai-gateway/pkg/repo"
)

// LimitDeps M6 RateLimit middleware 的依赖。
//
// PolicyCache：包了 QuotaPolicyProvider 一层 LRU+TTL，预解析 rule_json。
// Store：Redis 唯一实现（pkg/ratelimit.RedisStore），多 key 原子 ReserveBatch。
type LimitDeps struct {
	Store    ratelimit.Store
	Policies *ratelimit.PolicyCache
}

// 默认估算用：客户端没传 max_tokens 时按 4096 reserve，事后 M10 调账退多余。
const defaultMaxOutputTokens uint32 = 4096

// Limit 是 M6：tenant + apikey 双层、原子 RPM/TPM/RPS 限流。
//
// **顺序**：M5（拿到 ModelService）之后；M7（调上游）之前。
//
// **流程**：
//   1. 从 PolicyCache 拉两层 PolicyRule（tenant.QuotaPolicyID + apikey.QuotaPolicyID）
//   2. 按 additive 语义展开 buckets：default + per_model 都消耗
//   3. 估算 TPM cost（input chars/4 + max_tokens）
//   4. ReserveBatch 一次原子检查所有 buckets（all-or-nothing）
//   5. 拒绝 → 429 + Retry-After + X-RateLimit-* headers + 明确报错指明哪个 key
//   6. 通过 → 写 rc.RateLimit（M10 调账 + headers 数据） + 写 X-RateLimit-* headers
//
// **TPM 两阶段**：本 middleware reserve 估值；M10 拿真实 rc.Usage.Total 后调账（AdjustBatch）。
//
// **headers 总是返回**（即使没限流也写）：客户端能主动节流。
func Limit(deps LimitDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		ctx := rc.Ctx

		model := ""
		if rc.ModelService != nil {
			model = rc.ModelService.Model
		}
		var rawBody []byte
		if rc.Envelope != nil {
			rawBody = rc.Envelope.RawBytes
		}
		tpmCost := EnsureTPMEstimate(rc, rawBody)

		buckets, tpmKeys, err := buildBuckets(ctx, deps, &rc.Identity, model, tpmCost)
		if err != nil {
			abort(c, 500, domain.ErrUnknown, "ratelimit: build: "+err.Error())
			return
		}
		if len(buckets) == 0 {
			// 两层都没绑 policy：用户维度完全不限。estimate 已记录给 M7 endpoint check 用。
			c.Next()
			return
		}

		allowed, violated, err := deps.Store.ReserveBatch(ctx, buckets)
		if err != nil {
			abort(c, 500, domain.ErrUnknown, "ratelimit: reserve: "+err.Error())
			return
		}
		if !allowed {
			// 429：headers 用 violated bucket 数据填
			writeHeaders(c, violated.Limit, violated.Current, violated.RetryAfter)
			c.Header("Retry-After", strconv.Itoa(int(violated.RetryAfter.Seconds())))
			abort(c, 429, domain.ErrTransient,
				fmt.Sprintf("rate limit exceeded: %s (current %d / limit %d)",
					violated.Key, violated.Current, violated.Limit))
			return
		}

		// reserve 通过：补 TPM keys 到 state（M10 调账 + headers）+ 写 headers
		// （rc.RateLimit 在 EnsureTPMEstimate 已 init；这里追加 TPM keys）
		state := rc.RateLimit
		state.TPMBucketKeys = append(state.TPMBucketKeys, tpmKeys...)
		// 找最紧的 bucket（最小 Limit），snapshot 当前用量给 headers
		if tightest := pickTightestBucket(buckets); tightest != nil {
			st, err := deps.Store.Snapshot(ctx, *tightest)
			if err == nil {
				state.TightestBucketKey = tightest.Key
				state.TightestLimit = st.Limit
				state.TightestUsed = st.Used
				state.TightestResetAtSec = st.ResetAt.Unix()
				state.TightestWindowSec = int64(tightest.Window.Seconds())
				writeHeadersFromState(c, st)
			}
			// snapshot 失败不阻断请求；headers 缺失即可
		}
		c.Next()
	}
}

// EnsureTPMEstimate 保证 rc.RateLimit 存在且 ReservedTPM 已填。
//
// **谁调**：M6 总是调；M7 endpoint check 也可能调（M6 完全跳过的场景，比如用户没绑 policy）。
// 重复调是幂等的（已有就直接返回）。
//
// 返回当次估值（以及刚 init 的话也填到 rc.RateLimit）。
func EnsureTPMEstimate(rc *domain.RequestContext, rawBody []byte) uint32 {
	if rc.RateLimit != nil && rc.RateLimit.ReservedTPM > 0 {
		return rc.RateLimit.ReservedTPM
	}
	cost := EstimateTokens(rawBody, defaultMaxOutputTokens)
	if rc.RateLimit == nil {
		rc.RateLimit = &domain.RateLimitState{ReservedTPM: cost}
	} else {
		rc.RateLimit.ReservedTPM = cost
	}
	return cost
}

// buildBuckets 按 additive 语义把两层 PolicyRule 展开成 bucket 列表 + TPM keys。
//
// 命名约定：rl:user:<scope>:<subject>:<model_or_*>:<dim>
//   - scope = tenant | apikey
//   - subject = pin / api_key_id
//   - model_or_* = currentModel（per_model 命中）或 *（default fallback）
//   - dim = rpm | tpm | rps
func buildBuckets(
	ctx context.Context, deps LimitDeps, id *repo.UserIdentity, model string, tpmCost uint32,
) ([]ratelimit.Bucket, []string, error) {
	var buckets []ratelimit.Bucket
	var tpmKeys []string

	// 第 1 层：tenant
	if id.TenantQuotaPolicyID != nil {
		rule, err := deps.Policies.Get(ctx, *id.TenantQuotaPolicyID)
		if err != nil {
			return nil, nil, fmt.Errorf("tenant policy: %w", err)
		}
		buckets, tpmKeys = appendLayerBuckets(buckets, tpmKeys, "tenant", id.TenantID, rule, model, tpmCost)
	}
	// 第 2 层：apikey
	if id.APIKeyQuotaPolicyID != nil {
		rule, err := deps.Policies.Get(ctx, *id.APIKeyQuotaPolicyID)
		if err != nil {
			return nil, nil, fmt.Errorf("apikey policy: %w", err)
		}
		buckets, tpmKeys = appendLayerBuckets(buckets, tpmKeys, "apikey", id.APIKeyID, rule, model, tpmCost)
	}
	return buckets, tpmKeys, nil
}

// appendLayerBuckets 把一层 rule 展开（per_model + default 都加进 buckets，additive）。
func appendLayerBuckets(
	buckets []ratelimit.Bucket, tpmKeys []string,
	layer, subject string, rule *ratelimit.PolicyRule, model string, tpmCost uint32,
) ([]ratelimit.Bucket, []string) {
	if rule == nil {
		return buckets, tpmKeys
	}
	for _, sr := range rule.PickRulesAdditive(model) {
		if sr.Quota.RPM != nil && *sr.Quota.RPM > 0 {
			buckets = append(buckets, ratelimit.Bucket{
				Key:    fmt.Sprintf("rl:user:%s:%s:%s:rpm", layer, subject, sr.Scope),
				Limit:  *sr.Quota.RPM,
				Cost:   1,
				Window: time.Minute,
			})
		}
		if sr.Quota.TPM != nil && *sr.Quota.TPM > 0 {
			key := fmt.Sprintf("rl:user:%s:%s:%s:tpm", layer, subject, sr.Scope)
			buckets = append(buckets, ratelimit.Bucket{
				Key:    key,
				Limit:  *sr.Quota.TPM,
				Cost:   tpmCost,
				Window: time.Minute,
			})
			tpmKeys = append(tpmKeys, key)
		}
		if sr.Quota.RPS != nil && *sr.Quota.RPS > 0 {
			buckets = append(buckets, ratelimit.Bucket{
				Key:    fmt.Sprintf("rl:user:%s:%s:%s:rps", layer, subject, sr.Scope),
				Limit:  *sr.Quota.RPS,
				Cost:   1,
				Window: time.Second,
			})
		}
		// ConcurrentRequests：v0.5 deferred
	}
	return buckets, tpmKeys
}

// pickTightestBucket 选最紧的 bucket（最小 Limit）；nil = 列表为空。
//
// 用于：M6 reserve 后用这个 bucket 的状态填 X-RateLimit-* headers——客户端
// 知道"自己最容易撞到的天花板"还剩多少。
func pickTightestBucket(buckets []ratelimit.Bucket) *ratelimit.Bucket {
	if len(buckets) == 0 {
		return nil
	}
	tightest := &buckets[0]
	for i := 1; i < len(buckets); i++ {
		if buckets[i].Limit < tightest.Limit {
			tightest = &buckets[i]
		}
	}
	return tightest
}

// writeHeaders 直接写 X-RateLimit-* headers（429 路径用，传 violated 数据）。
func writeHeaders(c *gin.Context, limit, used uint32, resetIn time.Duration) {
	c.Header("X-RateLimit-Limit", strconv.FormatUint(uint64(limit), 10))
	var rem uint32
	if limit > used {
		rem = limit - used
	}
	c.Header("X-RateLimit-Remaining", strconv.FormatUint(uint64(rem), 10))
	c.Header("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(resetIn).Unix(), 10))
}

// writeHeadersFromState 200 路径用，从 Snapshot 拿绝对时间。
func writeHeadersFromState(c *gin.Context, st ratelimit.BucketState) {
	c.Header("X-RateLimit-Limit", strconv.FormatUint(uint64(st.Limit), 10))
	c.Header("X-RateLimit-Remaining", strconv.FormatUint(uint64(st.Remaining), 10))
	c.Header("X-RateLimit-Reset", strconv.FormatInt(st.ResetAt.Unix(), 10))
}
