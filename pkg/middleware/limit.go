package middleware

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
	"github.com/zereker/llm-gateway/pkg/repo"
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

// Limit 是 M6：主账号 + apikey 双层、原子 RPM/TPM/RPS 限流。
//
// **顺序**：M5（拿到 ModelService）之后；M7（调上游）之前。
//
// **流程**：
//  1. 从 PolicyCache 拉两层 PolicyRule（主账号 QuotaPolicyID + apikey.QuotaPolicyID）
//  2. 按 additive 语义展开 buckets：default + per_model 都消耗
//  3. 估算 TPM cost（input chars/4 + max_tokens）
//  4. ReserveBatch 一次原子检查所有 buckets（all-or-nothing）
//  5. 拒绝 → 429 + Retry-After + X-RateLimit-* headers + 明确报错指明哪个 key
//  6. 通过 → 写 rc.RateLimit（M10 调账 + headers 数据） + 写 X-RateLimit-* headers
//
// **TPM 两阶段（洋葱模型）**：本 middleware 走完整对称：
//   - pre-side：reserve 估值（ReserveBatch）
//   - c.Next() 等下游所有 middleware（含 M7 上游调用）跑完，rc.Usage 就绪
//   - post-side：adjustTPM 用真实 rc.Usage.Total - ReservedTPM 调账
//
// 这是 gin 洋葱模型的标准用法。adjustTPM 不在 M10 Tracing 里——M10 跟 RateLimit
// 没语义关系，硬塞进去只是早期 mistake；reserve 跟 adjust 是对称操作，住一处。
//
// **headers 总是返回**（即使没限流也写）：客户端能主动节流。
func Limit(deps LimitDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		ctx, end := startSpan(rc.Ctx, "llm-gateway.limit")
		defer end()
		rc.Ctx = ctx

		// 装配契约：M3 / M5 必须先跑（router middleware 链顺序）。fail-fast 跟
		// M5 / M7 同款 internal 错；不静默 fallback 防止 router 配错被埋。
		if rc.Envelope == nil || rc.ModelService == nil {
			abort(c, 500, domain.ErrUnknown, "internal: M3/M5 did not run before M6")
			return
		}

		// 1. 估 TPM cost + 展开两层 policy 的 buckets
		tpmCost := EnsureTPMEstimate(rc, rc.Envelope.RawBytes)
		buckets, tpmKeys, err := buildBuckets(ctx, deps, &rc.Identity, rc.ModelService.Model, tpmCost)
		if err != nil {
			abort(c, 500, domain.ErrUnknown, "ratelimit: build: "+err.Error())
			return
		}

		// 两层都没绑 policy：用户维度完全不限；estimate 已存给 M7 endpoint check 用
		if len(buckets) == 0 {
			c.Next()
			return
		}

		// 2. 原子 reserve：后端错 → 500；配额不够 → 429 + headers
		allowed, violated, err := deps.Store.ReserveBatch(ctx, buckets)
		if err != nil {
			abort(c, 500, domain.ErrUnknown, "ratelimit: reserve: "+err.Error())
			return
		}
		if !allowed {
			writeHeaders(c, violated.Limit, violated.Current, violated.RetryAfter)
			c.Header("Retry-After", strconv.Itoa(int(violated.RetryAfter.Seconds())))
			abort(c, 429, domain.ErrTransient,
				fmt.Sprintf("rate limit exceeded: %s (current %d / limit %d)",
					violated.Key, violated.Current, violated.Limit))
			return
		}

		// 3. 记 TPM keys（adjust 用）+ 写 X-RateLimit-* headers
		rc.RateLimit.TPMBucketKeys = append(rc.RateLimit.TPMBucketKeys, tpmKeys...)
		writeRateLimitState(c, ctx, deps.Store, rc.RateLimit, buckets)

		// 4. 执行下游 + post-side adjust（用真实 Usage 调账）
		c.Next()
		adjustTPM(rc, deps.Store)
	}
}

// writeRateLimitState 找最紧 bucket → snapshot 当前用量 → 填 RC + 写 X-RateLimit-* headers。
//
// best-effort：snapshot 失败不阻断请求，headers 缺失客户端只是没法主动节流，acceptable。
func writeRateLimitState(c *gin.Context, ctx context.Context, store ratelimit.Store, state *domain.RateLimitState, buckets []ratelimit.Bucket) {
	tightest := pickTightestBucket(buckets)
	if tightest == nil {
		return
	}
	st, err := store.Snapshot(ctx, *tightest)
	if err != nil {
		return
	}
	state.TightestBucketKey = tightest.Key
	state.TightestLimit = st.Limit
	state.TightestUsed = st.Used
	state.TightestResetAtSec = st.ResetAt.Unix()
	state.TightestWindowSec = int64(tightest.Window.Seconds())
	writeHeadersFromState(c, st)
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
// 命名约定：rl:quota:<scope>:<subject>:<model_or_*>:<dim>
//   - scope = account | apikey
//   - subject = 主账号 pin / api_key_id
//   - model_or_* = currentModel（per_model 命中）或 *（default fallback）
//   - dim = rpm | tpm | rps
func buildBuckets(ctx context.Context, deps LimitDeps, id *repo.UserIdentity, model string, tpmCost uint32) ([]ratelimit.Bucket, []string, error) {
	var buckets []ratelimit.Bucket
	var tpmKeys []string

	// 第 1 层：主账号（历史字段名仍为 account）
	if id.AccountQuotaPolicyID != nil {
		rule, err := deps.Policies.Get(ctx, *id.AccountQuotaPolicyID)
		if err != nil {
			return nil, nil, fmt.Errorf("account policy: %w", err)
		}
		buckets, tpmKeys = appendLayerBuckets(buckets, tpmKeys, "account", id.AccountID, rule, model, tpmCost)
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
				Key:    fmt.Sprintf("rl:quota:%s:%s:%s:rpm", layer, subject, sr.Scope),
				Limit:  *sr.Quota.RPM,
				Cost:   1,
				Window: time.Minute,
			})
		}
		if sr.Quota.TPM != nil && *sr.Quota.TPM > 0 {
			key := fmt.Sprintf("rl:quota:%s:%s:%s:tpm", layer, subject, sr.Scope)
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
				Key:    fmt.Sprintf("rl:quota:%s:%s:%s:rps", layer, subject, sr.Scope),
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

// adjustTPM 根据 rc.Usage.Total - rc.RateLimit.ReservedTPM 调账 pre-side 预扣的 TPM 桶。
//
// **调用契约**：仅 M6 主流程调；调用时 rc.RateLimit 已被 EnsureTPMEstimate 初始化，
// store 由 cmd 装配保证非 nil。所以**不**做 nil 检查；store / rc.RateLimit 为 nil
// 是装配 bug 该 panic（M9 Recover 兜底）。
//
// **真实合法 noop 路径**：
//   - TPMBucketKeys 空 → 没绑 policy（reserve 没扣，不必调账）
//   - rc.Usage 为 nil → M7 上游全失败没拿到 usage（不扣账保险，下次窗口自动 reset）
//
// **best-effort**：AdjustBatch 失败仅静默；下次请求看到的剩余配额偏不准，acceptable。
//
// **窗口写死 1min**：M6 buildBuckets 里 TPM bucket Window 也是 time.Minute；两边
// 必须一致。如果将来 TPM 改 5min 等，要把 window 也存进 RateLimitState。
func adjustTPM(rc *domain.RequestContext, store ratelimit.Store) {
	if len(rc.RateLimit.TPMBucketKeys) == 0 || rc.Usage == nil {
		return
	}
	delta := int32(rc.Usage.Total) - int32(rc.RateLimit.ReservedTPM)
	if delta == 0 {
		return
	}
	adjustments := make([]ratelimit.BucketAdjust, len(rc.RateLimit.TPMBucketKeys))
	for i, k := range rc.RateLimit.TPMBucketKeys {
		adjustments[i] = ratelimit.BucketAdjust{
			Key:    k,
			Delta:  delta,
			Window: time.Minute,
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = store.AdjustBatch(ctx, adjustments) // best-effort；失败 log 等下版加
}
