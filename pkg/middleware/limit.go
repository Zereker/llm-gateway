package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/metric"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
)

// QuotaPolicies is the port through which M6 pulls the two quota-policy
// layers (account + api-key).
//
// Implementers (pkg/ratelimit.PolicyCache) implement it + caching for their
// own domain.
type QuotaPolicies interface {
	Get(ctx context.Context, id int64) (*ratelimit.PolicyRule, error)
}

// LimitOption configures the Limit middleware (same interface-Option pattern as otelgin v0.68.0).
type LimitOption interface {
	apply(*limitConfig)
}

type limitOptionFunc func(*limitConfig)

func (f limitOptionFunc) apply(c *limitConfig) { f(c) }

type limitConfig struct {
	store    ratelimit.Store
	policies QuotaPolicies
}

// WithLimitStore injects a ratelimit.Store implementation. Required.
func WithLimitStore(s ratelimit.Store) LimitOption {
	return limitOptionFunc(func(c *limitConfig) { c.store = s })
}

// WithLimitPolicies injects a QuotaPolicies implementation. Required.
func WithLimitPolicies(p QuotaPolicies) LimitOption {
	return limitOptionFunc(func(c *limitConfig) { c.policies = p })
}

// Limit is M6: two user-side layers (account + apikey) with additive RPM/RPS
// pre-deduction + TPM post-deduction.
//
// **Order**: after M5 (ModelService obtained); before M7 (calls upstream).
//
// **Flow** (docs/04 §2 §7):
//  1. Pull two PolicyRule layers from PolicyCache
//  2. Expand into buckets with additive semantics (both default and per_model consume)
//  3. **Only build RPM / RPS buckets** for ReserveBatch; TPM buckets are stashed separately for post-side
//  4. ReserveBatch atomically checks RPM/RPS
//  5. Reject → 429 + Retry-After header (does **not** write X-RateLimit-*) + details carry the exceeded dimension
//  6. Pass → c.Next()
//  7. post-side: once rc.Usage is ready → ChargeBatch(TPM, cost=Usage.Total); overflow tags the tpm_overflow metric
//
// **TPM post-deduction failure semantics** (docs/04 §7):
//   - rc.Usage == nil → TPM not deducted (usage extractor coverage gap)
//   - Charge write exceeds limit → does not change this response; records llm_gateway_tpm_overflow_total
func Limit(opts ...LimitOption) gin.HandlerFunc {
	cfg := limitConfig{}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	// No Store or Policies → no-op pass-through (suitable for dev / rate-limit-free deployments)
	if cfg.store == nil || cfg.policies == nil {
		return func(c *gin.Context) { c.Next() }
	}
	tracer := otel.GetTracerProvider().Tracer(ScopeName)

	return func(c *gin.Context) {
		ctx, span := tracer.Start(c.Request.Context(), "ratelimit.reserve")
		defer span.End()
		c.Request = c.Request.WithContext(ctx)

		rc := GetRequestContext(c)
		if rc.Envelope == nil || rc.ModelService == nil {
			abortWithCode(c, 500, domain.ErrUnknown, domain.ErrCodeInternalError,
				"internal: M3/M5 did not run before M6")
			return
		}

		// Expand the two policy layers → (reserve buckets = RPM/RPS, charge buckets = TPM)
		reserveBuckets, tpmBuckets, err := buildUserBuckets(ctx, cfg.policies, &rc.Identity, rc.ModelService.Model)
		if err != nil {
			metric.Inc(metric.PolicyCacheTotal, "layer", "any", "result", "error")
			slog.ErrorContext(ctx, "m6: rate-limit policy build failed", "err", err)
			abortWithCode(c, 500, domain.ErrUnknown, domain.ErrCodeInternalError,
				"rate limit policy unavailable")
			return
		}

		// No policy bound at all → skip rate limiting; only leave tpm buckets for
		// post-side (which is empty too in this case)
		if len(reserveBuckets) == 0 && len(tpmBuckets) == 0 {
			c.Next()
			return
		}

		// RPM/RPS pre-deduction (docs/04 §5)
		if len(reserveBuckets) > 0 {
			allowed, violated, rerr := cfg.store.ReserveBatch(ctx, reserveBuckets)
			if rerr != nil {
				// fail-closed (docs/04 §8). Details go to logs only, never the
				// response body — consistent with auth dependency failures, to
				// avoid leaking Redis / driver internal errors (host / topology)
				// to the client.
				metric.Inc(metric.RateLimitDecisionsTotal, "scope", "user", "dimension", "any", "result", "error")
				slog.ErrorContext(ctx, "ratelimit: reserve failed", "err", rerr)
				abortWithCode(c, 503, domain.ErrTransient, domain.ErrCodeDependencyUnavailable,
					"rate limiter unavailable")
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

		// Stash the TPM bucket keys for post-side (rc.RateLimit also carries this
		// along for metric / observability)
		if len(tpmBuckets) > 0 {
			if rc.RateLimit == nil {
				rc.RateLimit = &domain.RateLimitState{}
			}
			for _, b := range tpmBuckets {
				rc.RateLimit.TPMBucketKeys = append(rc.RateLimit.TPMBucketKeys, b.Key)
			}
		}

		// Run downstream + post-side TPM charge
		c.Next()
		chargeTPM(rc, cfg.store, tpmBuckets)
	}
}

// chargeTPM does the TPM bucket post-deduction: cost = rc.Usage.Total.
//
// Failure semantics (docs/04 §7 §8):
//   - rc.Usage == nil → not deducted (usage extractor didn't get one)
//   - Charge fails → does not change the response; records a metric
//   - Exceeds limit after write → records tpm_overflow_total; does not affect this request
func chargeTPM(rc *domain.RequestContext, store ratelimit.Store, tpmBuckets []ratelimit.Bucket) {
	if rc.Usage == nil || rc.Usage.Total <= 0 || len(tpmBuckets) == 0 {
		return
	}
	cost := uint32(rc.Usage.Total)
	if cost == 0 {
		return
	}
	// Inject cost into each bucket
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
// buildUserBuckets: expands the two policy layers into (RPM/RPS buckets, TPM buckets)
// using additive semantics
// =============================================================================

// buildUserBuckets takes the two PolicyRule layers and expands them into:
//   - reserveBuckets: RPM + RPS, used by ReserveBatch (cost=1)
//   - tpmBuckets:     TPM, used by post-side ChargeBatch (cost is injected with
//     the real value by chargeTPM)
//
// Naming (docs/04 §6): rl:quota:<layer>:<subject>:<scope>:<dim>
func buildUserBuckets(
	ctx context.Context,
	policies QuotaPolicies,
	id *domain.UserIdentity,
	model string,
) (reserveBuckets, tpmBuckets []ratelimit.Bucket, err error) {
	// Layer 1: account
	if id.AccountQuotaPolicyID != nil {
		rule, err := policies.Get(ctx, *id.AccountQuotaPolicyID)
		if err != nil {
			return nil, nil, fmt.Errorf("account policy: %w", err)
		}
		reserveBuckets, tpmBuckets = appendLayer(reserveBuckets, tpmBuckets, "account", id.AccountID, rule, model)
	}
	// Layer 2: apikey
	if id.APIKeyQuotaPolicyID != nil {
		rule, err := policies.Get(ctx, *id.APIKeyQuotaPolicyID)
		if err != nil {
			return nil, nil, fmt.Errorf("apikey policy: %w", err)
		}
		reserveBuckets, tpmBuckets = appendLayer(reserveBuckets, tpmBuckets, "apikey", id.APIKeyID, rule, model)
	}
	return reserveBuckets, tpmBuckets, nil
}

// appendLayer expands a single rule layer (per_model + default additive):
//   - RPM / RPS → reserveBuckets
//   - TPM       → tpmBuckets (cost injected with the real value at chargeTPM time)
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
				Cost:   0, // placeholder; chargeTPM injects rc.Usage.Total
				Window: time.Minute,
			})
		}
	}
	return reserve, tpm
}

// layerFromKey extracts the layer (account | apikey | endpoint) from a bucket key.
//
// key looks like "rl:quota:account:..." / "rl:quota:apikey:..." / "rl:endpoint:..."
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

// dimensionFromKey extracts the dimension (rpm | rps | tpm) from a bucket key.
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
