package middleware

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/metric"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
)

// RateLimitStore M6 用户侧 RPM/RPS 前扣 + TPM 后扣依赖的存储 port。
//
// 接口是 middleware-owned；实现者（pkg/ratelimit.RedisStore）按自己的领域写代码、
// 顺便满足这个 port。ratelimit.Bucket 等 value type 仍由 ratelimit 包定义——
// 中间件只反转抽象归属，不强迫 value type 搬家。
type RateLimitStore interface {
	ReserveBatch(ctx context.Context, buckets []ratelimit.Bucket) (allowed bool, violated *ratelimit.BucketViolation, err error)
	ChargeBatch(ctx context.Context, buckets []ratelimit.Bucket) ([]ratelimit.BucketChargeResult, error)
}

// QuotaPolicies M6 拉取主账号 / api-key 两层 quota policy 的 port。
//
// 实现者（pkg/ratelimit.PolicyCache）按自己的领域实现 + 缓存。
type QuotaPolicies interface {
	Get(ctx context.Context, id int64) (*ratelimit.PolicyRule, error)
}

// LimitOption 配置 Limit middleware（otelgin v0.68.0 同款 interface-Option）。
type LimitOption interface {
	apply(*limitConfig)
}

type limitOptionFunc func(*limitConfig)

func (f limitOptionFunc) apply(c *limitConfig) { f(c) }

type limitConfig struct {
	store          RateLimitStore
	policies       QuotaPolicies
	tracerProvider oteltrace.TracerProvider
}

// WithLimitStore 注入 RateLimitStore 实现。必填。
func WithLimitStore(s RateLimitStore) LimitOption {
	return limitOptionFunc(func(c *limitConfig) { c.store = s })
}

// WithLimitPolicies 注入 QuotaPolicies 实现。必填。
func WithLimitPolicies(p QuotaPolicies) LimitOption {
	return limitOptionFunc(func(c *limitConfig) { c.policies = p })
}

// WithLimitTracerProvider 注入 OTel TracerProvider；nil 时启动期退到 otel.GetTracerProvider()。
func WithLimitTracerProvider(tp oteltrace.TracerProvider) LimitOption {
	return limitOptionFunc(func(c *limitConfig) {
		if tp != nil {
			c.tracerProvider = tp
		}
	})
}

// Limit 是 M6：用户侧两层（account + apikey）+ additive RPM/RPS 前扣 + TPM 后扣。
//
// **顺序**：M5（拿到 ModelService）之后；M7（调上游）之前。
//
// **流程**（docs/04 §2 §7）：
//  1. 从 PolicyCache 拉两层 PolicyRule
//  2. 按 additive 语义展开 buckets（default + per_model 都消耗）
//  3. **只构造 RPM / RPS bucket** 给 ReserveBatch；TPM bucket 单独存给 post-side
//  4. ReserveBatch 原子检查 RPM/RPS
//  5. 拒绝 → 429 + Retry-After header（**不**写 X-RateLimit-*）+ details 带超限维度
//  6. 通过 → c.Next()
//  7. post-side：rc.Usage 就绪 → ChargeBatch(TPM, cost=Usage.Total)；超限标 tpm_overflow metric
//
// **TPM 后扣失败语义**（docs/04 §7）：
//   - rc.Usage == nil → 不扣 TPM（usage extractor 覆盖率问题）
//   - Charge 写入超限 → 不改本次响应；记 llm_gateway_tpm_overflow_total
func Limit(opts ...LimitOption) gin.HandlerFunc {
	cfg := limitConfig{}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	// 没有 Store 或 Policies → no-op pass-through（适合 dev / 无限流部署）
	if cfg.store == nil || cfg.policies == nil {
		return func(c *gin.Context) { c.Next() }
	}
	if cfg.tracerProvider == nil {
		cfg.tracerProvider = otel.GetTracerProvider()
	}
	tracer := cfg.tracerProvider.Tracer(ScopeName)

	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		ctx, span := tracer.Start(rc.Ctx, "ratelimit.reserve")
		defer span.End()
		rc.Ctx = ctx

		if rc.Envelope == nil || rc.ModelService == nil {
			abortWithCode(c, 500, domain.ErrUnknown, domain.ErrCodeInternalError,
				"internal: M3/M5 did not run before M6")
			return
		}

		// 展开两层 policy → (reserve buckets = RPM/RPS, charge buckets = TPM)
		reserveBuckets, tpmBuckets, err := buildUserBuckets(ctx, cfg.policies, &rc.Identity, rc.ModelService.Model)
		if err != nil {
			metric.Inc(metric.PolicyCacheTotal, "layer", "any", "result", "error")
			abortWithCode(c, 500, domain.ErrUnknown, domain.ErrCodeInternalError,
				"ratelimit: build: "+err.Error())
			return
		}

		// 没绑任何 policy → 跳过限流；只把 tpm buckets 留给 post-side（其实也为空）
		if len(reserveBuckets) == 0 && len(tpmBuckets) == 0 {
			c.Next()
			return
		}

		// RPM/RPS 前扣（docs/04 §5）
		if len(reserveBuckets) > 0 {
			allowed, violated, rerr := cfg.store.ReserveBatch(ctx, reserveBuckets)
			if rerr != nil {
				// fail-closed（docs/04 §8）
				metric.Inc(metric.RateLimitDecisionsTotal, "scope", "user", "dimension", "any", "result", "error")
				abortWithCode(c, 503, domain.ErrTransient, domain.ErrCodeDependencyUnavailable,
					"ratelimit: reserve: "+rerr.Error())
				return
			}
			if !allowed {
				metric.Inc(metric.RateLimitDecisionsTotal, "scope", "user", "dimension", dimensionFromKey(violated.Key), "result", "violated")
				c.Header("Retry-After", strconv.Itoa(int(violated.RetryAfter.Seconds())))
				abortWithDetails(c, 429, domain.ErrRateLimit, domain.ErrCodeRateLimitExceeded,
					fmt.Sprintf("rate limit exceeded: %s (current %d / limit %d)",
						violated.Key, violated.Current, violated.Limit),
					map[string]any{
						"bucket_key":      violated.Key,
						"limit":           violated.Limit,
						"current":         violated.Current,
						"retry_after_sec": int(violated.RetryAfter.Seconds()),
					},
				)
				return
			}
			metric.Inc(metric.RateLimitDecisionsTotal, "scope", "user", "dimension", "rpm_rps", "result", "allowed")
		}

		// 暂存 TPM bucket key 给 post-side（rc.RateLimit 也带过去给 metric / observability）
		if len(tpmBuckets) > 0 {
			if rc.RateLimit == nil {
				rc.RateLimit = &domain.RateLimitState{}
			}
			for _, b := range tpmBuckets {
				rc.RateLimit.TPMBucketKeys = append(rc.RateLimit.TPMBucketKeys, b.Key)
			}
		}

		// 执行下游 + post-side TPM charge
		c.Next()
		chargeTPM(rc, cfg.store, tpmBuckets)
	}
}

// chargeTPM 后扣 TPM bucket：cost = rc.Usage.Total。
//
// 失败语义（docs/04 §7 §8）：
//   - rc.Usage == nil → 不扣（usage extractor 没拿到）
//   - Charge 失败 → 不改响应；记 metric
//   - 写入后超限 → 记 tpm_overflow_total；不影响本次
func chargeTPM(rc *domain.RequestContext, store RateLimitStore, tpmBuckets []ratelimit.Bucket) {
	if rc.Usage == nil || rc.Usage.Total <= 0 || len(tpmBuckets) == 0 {
		return
	}
	cost := uint32(rc.Usage.Total)
	if cost == 0 {
		return
	}
	// cost 注入到每个 bucket
	for i := range tpmBuckets {
		tpmBuckets[i].Cost = cost
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	results, err := store.ChargeBatch(ctx, tpmBuckets)
	if err != nil {
		metric.Inc(metric.RateLimitChargeTotal, "dimension", "tpm", "result", "error")
		return
	}
	metric.Inc(metric.RateLimitChargeTotal, "dimension", "tpm", "result", "ok")
	for _, r := range results {
		if r.Overflow {
			metric.Inc(metric.TPMOverflowTotal,
				"layer", layerFromKey(r.Key),
				"dimension", "tpm",
			)
		}
	}
}

// =============================================================================
// buildUserBuckets：按 additive 语义展开两层 policy → (RPM/RPS 桶, TPM 桶)
// =============================================================================

// buildUserBuckets 拿两层 PolicyRule 展开成：
//   - reserveBuckets：RPM + RPS，用于 ReserveBatch（cost=1）
//   - tpmBuckets：    TPM，用于 post-side ChargeBatch（cost 由 chargeTPM 注入真实值）
//
// 命名（docs/04 §6）：rl:quota:<layer>:<subject>:<scope>:<dim>
func buildUserBuckets(
	ctx context.Context,
	policies QuotaPolicies,
	id *domain.UserIdentity,
	model string,
) (reserveBuckets, tpmBuckets []ratelimit.Bucket, err error) {
	// 第 1 层：主账号
	if id.AccountQuotaPolicyID != nil {
		rule, err := policies.Get(ctx, *id.AccountQuotaPolicyID)
		if err != nil {
			return nil, nil, fmt.Errorf("account policy: %w", err)
		}
		reserveBuckets, tpmBuckets = appendLayer(reserveBuckets, tpmBuckets, "account", id.AccountID, rule, model)
	}
	// 第 2 层：apikey
	if id.APIKeyQuotaPolicyID != nil {
		rule, err := policies.Get(ctx, *id.APIKeyQuotaPolicyID)
		if err != nil {
			return nil, nil, fmt.Errorf("apikey policy: %w", err)
		}
		reserveBuckets, tpmBuckets = appendLayer(reserveBuckets, tpmBuckets, "apikey", id.APIKeyID, rule, model)
	}
	return reserveBuckets, tpmBuckets, nil
}

// appendLayer 把一层 rule 展开（per_model + default additive）：
//   - RPM / RPS → reserveBuckets
//   - TPM       → tpmBuckets（cost 在 chargeTPM 时注入真实值）
func appendLayer(
	reserve, tpm []ratelimit.Bucket,
	layer, subject string,
	rule *ratelimit.PolicyRule, model string,
) (out []ratelimit.Bucket, outTPM []ratelimit.Bucket) {
	if rule == nil {
		return reserve, tpm
	}
	for _, sr := range rule.PickRulesAdditive(model) {
		if sr.Quota.RPM != nil && *sr.Quota.RPM > 0 {
			reserve = append(reserve, ratelimit.Bucket{
				Key:    fmt.Sprintf("rl:quota:%s:%s:%s:rpm", layer, subject, sr.Scope),
				Limit:  *sr.Quota.RPM,
				Cost:   1,
				Window: time.Minute,
			})
		}
		if sr.Quota.RPS != nil && *sr.Quota.RPS > 0 {
			reserve = append(reserve, ratelimit.Bucket{
				Key:    fmt.Sprintf("rl:quota:%s:%s:%s:rps", layer, subject, sr.Scope),
				Limit:  *sr.Quota.RPS,
				Cost:   1,
				Window: time.Second,
			})
		}
		if sr.Quota.TPM != nil && *sr.Quota.TPM > 0 {
			tpm = append(tpm, ratelimit.Bucket{
				Key:    fmt.Sprintf("rl:quota:%s:%s:%s:tpm", layer, subject, sr.Scope),
				Limit:  *sr.Quota.TPM,
				Cost:   0, // 占位；chargeTPM 注入 rc.Usage.Total
				Window: time.Minute,
			})
		}
	}
	return reserve, tpm
}

// layerFromKey 从 bucket key 抠 layer（account | apikey | endpoint）。
//
// key 形如 "rl:quota:account:..." / "rl:quota:apikey:..." / "rl:endpoint:..."
func layerFromKey(key string) string {
	if len(key) >= 14 && key[:9] == "rl:quota:" {
		if key[9:16] == "account" {
			return "account"
		}
		if key[9:15] == "apikey" {
			return "apikey"
		}
	}
	if len(key) >= 12 && key[:12] == "rl:endpoint:" {
		return "endpoint"
	}
	return "unknown"
}

// dimensionFromKey 从 bucket key 抠 dimension（rpm | rps | tpm）。
func dimensionFromKey(key string) string {
	switch {
	case len(key) >= 4 && key[len(key)-4:] == ":rpm":
		return "rpm"
	case len(key) >= 4 && key[len(key)-4:] == ":rps":
		return "rps"
	case len(key) >= 4 && key[len(key)-4:] == ":tpm":
		return "tpm"
	}
	return "unknown"
}
