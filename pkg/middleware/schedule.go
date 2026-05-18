package middleware

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"

	"github.com/zereker/llm-gateway/pkg/adapter"
	"github.com/zereker/llm-gateway/pkg/contentlog"
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/metric"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
	"github.com/zereker/llm-gateway/pkg/schedule"
	"github.com/zereker/llm-gateway/pkg/schedule/eligibility"
	"github.com/zereker/llm-gateway/pkg/translator"
	"github.com/zereker/llm-gateway/pkg/upstream"
)

// MaxFallbackModels X-Gateway-Fallback-Models header 允许的最多 model 数（docs/03 §5）。
const MaxFallbackModels = 3

// EndpointReader M7 用：按 (model, group) 拉候选 endpoints。
//
// 接口是 middleware-owned；SQL 适配见 cmd/gateway/middleware_adapters.go 里的 adaptEndpoints。
type EndpointReader interface {
	ListForModel(ctx context.Context, model, group string) ([]*domain.Endpoint, error)
}

// Scheduler M7 端点选路 port——middleware-owned，不依赖 pkg/schedule 自己的接口。
//
// 实现者（pkg/schedule.Scheduler 同名接口的实现）按自己的领域写代码、顺便满足这个 port。
// schedule.Request / schedule.Result 是 value type，留在 schedule 包定义；这里只反转
// 抽象归属。
type Scheduler interface {
	Pick(ctx context.Context, req *schedule.Request) (*domain.Endpoint, error)
	Report(ctx context.Context, ep *domain.Endpoint, result schedule.Result)
}

// Sender M7 实际调上游 + 流式 forward 的 port——middleware-owned。
//
// 实现者（pkg/upstream.Sender concrete 类型）按自己的领域写代码、顺便满足这个 port。
type Sender interface {
	Send(ctx context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope, srcBody []byte) (upstream.Outcome, error)
	Forward(ctx context.Context, w http.ResponseWriter, ep *domain.Endpoint, resp *http.Response, handler translator.ResponseHandler) upstream.ForwardResult
}

// ScheduleOption 配置 Schedule middleware（otelgin v0.68.0 同款 interface-Option）。
type ScheduleOption interface {
	apply(*scheduleConfig)
}

type scheduleOptionFunc func(*scheduleConfig)

func (f scheduleOptionFunc) apply(c *scheduleConfig) { f(c) }

type scheduleConfig struct {
	endpoints     EndpointReader
	catalog       ModelCatalog
	subscriptions SubscriptionChecker
	scheduler     Scheduler
	sender        Sender
	rateStore     RateLimitStore // 可空：跳过 endpoint quota
	maxAttempts   int
}

// WithEndpointReader 注入 EndpointReader。必填。
func WithEndpointReader(r EndpointReader) ScheduleOption {
	return scheduleOptionFunc(func(c *scheduleConfig) { c.endpoints = r })
}

// WithFallbackCatalog 注入 ModelCatalog 用于 fallback model 重校验（docs/03 §1）。必填。
//
// 注意：这跟 M5 的 ModelCatalog 是同一接口，但需要单独注入到 M7（每个 fallback model
// 都要重新走 catalog + subscription 校验，不能复用 M5 的结果）。
func WithFallbackCatalog(c ModelCatalog) ScheduleOption {
	return scheduleOptionFunc(func(cfg *scheduleConfig) { cfg.catalog = c })
}

// WithFallbackSubscriptionChecker 注入 SubscriptionChecker 用于 fallback model 校验。必填。
func WithFallbackSubscriptionChecker(s SubscriptionChecker) ScheduleOption {
	return scheduleOptionFunc(func(cfg *scheduleConfig) { cfg.subscriptions = s })
}

// WithScheduler 注入 Scheduler 实现。必填。
func WithScheduler(s Scheduler) ScheduleOption {
	return scheduleOptionFunc(func(c *scheduleConfig) { c.scheduler = s })
}

// WithSender 注入 Sender 实现。必填。
func WithSender(s Sender) ScheduleOption {
	return scheduleOptionFunc(func(c *scheduleConfig) { c.sender = s })
}

// WithEndpointRateStore 注入 endpoint 维度的 RateLimitStore（选中后 reserve + TPM charge）。
//
// 不传 = 跳过 endpoint quota（适合 dev / 不配 endpoint quota 的部署）。
func WithEndpointRateStore(s RateLimitStore) ScheduleOption {
	return scheduleOptionFunc(func(c *scheduleConfig) { c.rateStore = s })
}

// WithMaxAttempts 设置全局尝试上限；0 = 默认 3。
//
// header X-Gateway-Max-Attempts 可往**更小**方向覆盖；不能比这个值更大。
func WithMaxAttempts(n int) ScheduleOption {
	return scheduleOptionFunc(func(c *scheduleConfig) { c.maxAttempts = n })
}

// Schedule 是 M7：
//
//	for model in [request.model] + fallback_models:
//	    re-do M5 (catalog + subscription)
//	    cands := EndpointReader.ListForModel(model, group)
//	    cands := eligibility.Filter(cands, envelope)
//	    for attempts < max:
//	        ep := Scheduler.Pick(ctx, Request{Candidates, ExcludeIDs})
//	        if ep == nil: break
//	        outcome := Sender.Send(ctx, ep, env, body)
//	        Scheduler.Report(ctx, ep, outcome.ToScheduleResult())
//	        if invalid: abort 400
//	        if success: Forward + return
//
//	abort 503
//
// 详见 docs/architecture/03-endpoint-scheduling.md §1。
func Schedule(opts ...ScheduleOption) gin.HandlerFunc {
	cfg := scheduleConfig{}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	if cfg.endpoints == nil {
		panic("middleware.Schedule: WithEndpointReader required")
	}
	if cfg.catalog == nil {
		panic("middleware.Schedule: WithFallbackCatalog required")
	}
	if cfg.subscriptions == nil {
		panic("middleware.Schedule: WithFallbackSubscriptionChecker required")
	}
	if cfg.scheduler == nil {
		panic("middleware.Schedule: WithScheduler required")
	}
	if cfg.sender == nil {
		panic("middleware.Schedule: WithSender required")
	}
	maxAttempts := cfg.maxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	tracer := otel.GetTracerProvider().Tracer(ScopeName)

	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		ctx, span := tracer.Start(rc.Ctx, "schedule.pick")
		defer span.End()
		rc.Ctx = ctx

		if rc.Envelope == nil || rc.ModelService == nil {
			abortWithCode(c, 500, domain.ErrUnknown, domain.ErrCodeInternalError,
				"internal: M3/M5 did not run before M7")
			return
		}

		// 注入 content log enrichment（Logger 通过 ctx 拿请求元信息；docs/05 §2）
		ctx = contentlog.EnrichCtx(ctx, contentlog.RequestEnrich{
			RequestID:    rc.RequestID,
			TraceID:      TraceIDFromCtx(ctx),
			AccountID:    rc.Identity.AccountID,
			APIKeyID:     rc.Identity.APIKeyID,
			SubAccountID: rc.Identity.SubAccountID,
			Model:        rc.ModelService.Model,
			Protocol:     rc.Envelope.SourceProtocol.String(),
			Modality:     rc.Envelope.Modality.String(),
		})
		rc.Ctx = ctx

		// 本请求实际允许的 attempts（header override 仅允许更紧）
		attemptsCap := maxAttempts
		if h := parseMaxAttempts(c); h > 0 && h < attemptsCap {
			attemptsCap = h
		}

		// model 序列：primary + fallback
		fallbacks := parseFallbackModels(c)
		if len(fallbacks) > MaxFallbackModels {
			fallbacks = fallbacks[:MaxFallbackModels]
		}
		modelSeq := append([]string{rc.ModelService.Model}, fallbacks...)

		// per-request cache 避免对同 model 重复查 catalog/subscription
		msCache := make(map[string]*domain.ModelService, len(modelSeq))
		msCache[rc.ModelService.Model] = rc.ModelService
		subCache := make(map[string]bool, len(modelSeq))
		subCache[rc.ModelService.Model] = true // M5 已经验过 primary

		excluded := make(map[int64]struct{}, attemptsCap)
		decisions := make([]domain.Attempt, 0, attemptsCap)
		responseStarted := false
		var totalAttempts int

		startSched := time.Now()
		defer func() {
			metric.Observe(metric.SchedulingDurationSeconds, time.Since(startSched).Seconds(),
				"model", rc.ModelService.Model,
				"attempts", strconv.Itoa(totalAttempts),
			)
			if len(decisions) > 0 {
				rc.SchedulingDecision = &domain.SchedulingDecision{
					Model:       rc.ModelService.Model,
					RoutedModel: routedModelOf(rc),
					UserGroup:   rc.Identity.Group,
					Attempts:    decisions,
					DurationMs:  time.Since(startSched).Milliseconds(),
				}
			}
		}()

		for modelIdx, model := range modelSeq {
			role := domain.AttemptRolePrimary
			if modelIdx > 0 {
				role = domain.AttemptRoleFallback
			}

			// 每个 fallback model 重新做 M5（docs/03 §1：不能复用原 M5 结果）
			ms, ok, err := resolveModel(ctx, cfg.catalog, cfg.subscriptions, rc.Identity.AccountID, model, msCache, subCache)
			if err != nil {
				// 依赖故障，整请求中止
				if !responseStarted {
					abortWithCode(c, 503, domain.ErrTransient, domain.ErrCodeDependencyUnavailable,
						"schedule: re-validate model "+model+": "+err.Error())
				}
				return
			}
			if !ok {
				continue // 模型不存在或未订阅 → 试下一个 fallback
			}

			// 拉候选 + eligibility 过滤
			rawCands, err := cfg.endpoints.ListForModel(ctx, model, rc.Identity.Group)
			if err != nil {
				if !responseStarted {
					abortWithCode(c, 503, domain.ErrTransient, domain.ErrCodeDependencyUnavailable,
						"list endpoints: "+err.Error())
				}
				return
			}
			elgStart := time.Now()
			elgResult := eligibility.Filter(rawCands, rc.Envelope, adapterRegistryLookup{}, translatorRegistryLookup{})
			metric.Observe(metric.EligibilityDurationSeconds, time.Since(elgStart).Seconds(), "model", model)
			metric.Observe(metric.SchedulerCandidates, float64(len(rawCands)), "model", model, "stage", "list")
			metric.Observe(metric.SchedulerCandidates, float64(len(elgResult.Eligible)), "model", model, "stage", "eligible")

			if len(elgResult.Eligible) == 0 {
				continue
			}

			// 构造初始 Candidates（EffectiveWeight = static Endpoint.Weight）
			candidates := make([]schedule.Candidate, len(elgResult.Eligible))
			for i, ep := range elgResult.Eligible {
				candidates[i] = schedule.Candidate{
					Endpoint:        ep,
					EffectiveWeight: float64(ep.Weight),
				}
			}

			// 单 model 内 attempt loop
			for totalAttempts < attemptsCap {
				ep, err := cfg.scheduler.Pick(ctx, &schedule.Request{
					Model:      model,
					Group:      rc.Identity.Group,
					Candidates: candidates,
					ExcludeIDs: excluded,
				})
				if err != nil {
					if !responseStarted {
						abortWithCode(c, 503, domain.ErrTransient, domain.ErrCodeNoEndpointAvailable,
							"schedule: pick: "+err.Error())
					}
					return
				}
				if ep == nil {
					break // 当前 model 候选耗尽，试下一个 fallback model
				}
				rc.Endpoint = ep
				totalAttempts++

				// 选中 endpoint 后做 RPM/RPS reserve（docs/04 §10）。
				// 超限 → 反馈 capacity + 排除 ep + 继续 Pick 下一个
				if cfg.rateStore != nil {
					if epBuckets := schedule.EndpointReserveBuckets(ep); len(epBuckets) > 0 {
						allowed, violated, rerr := cfg.rateStore.ReserveBatch(ctx, epBuckets)
						if rerr != nil {
							// fail-open：把 ep 当不可用，try 下一个（不阻塞整请求）
							cfg.scheduler.Report(ctx, ep, schedule.Result{Class: schedule.ClassCapacity, Reason: "endpoint reserve: " + rerr.Error()})
							excluded[ep.ID] = struct{}{}
							continue
						}
						if !allowed {
							cfg.scheduler.Report(ctx, ep, schedule.Result{Class: schedule.ClassCapacity, Reason: "endpoint quota exhausted: " + violated.Key})
							excluded[ep.ID] = struct{}{}
							metric.Inc(metric.RateLimitDecisionsTotal, "scope", "endpoint", "dimension", dimensionFromKey(violated.Key), "result", "violated")
							continue
						}
					}
				}

				outcome, callErr := cfg.sender.Send(ctx, ep, rc.Envelope, rc.Envelope.RawBytes)
				cfg.scheduler.Report(ctx, ep, outcome.ToScheduleResult())

				// 记录 attempt
				decisions = append(decisions, domain.Attempt{
					Index:       totalAttempts,
					Model:       model,
					EndpointID:  strconv.FormatInt(ep.ID, 10),
					AttemptRole: role,
					LatencyMs:   outcome.Latency.Milliseconds(),
					ErrorClass:  outcome.Class.String(),
					Started:     time.Now().Add(-outcome.Latency),
				})

				// metric: scheduler_attempts_total
				routedModel := model
				metric.Inc(metric.SchedulerAttemptsTotal,
					"model", rc.ModelService.Model,
					"routed_model", routedModel,
					"vendor", ep.Vendor,
					"endpoint_id", strconv.FormatInt(ep.ID, 10),
					"attempt_role", string(role),
					"result", outcome.Class.String(),
					"error_class", outcome.Class.String(),
				)

				// translator request 失败 → invalid，不重试任何 ep
				if errors.Is(callErr, upstream.ErrInvalidRequest) {
					decisions[len(decisions)-1].Outcome = domain.AttemptFail
					abortWithCode(c, 400, domain.ErrInvalid, domain.ErrCodeInvalidRequest, outcome.Reason)
					return
				}

				if outcome.Success() {
					rc.RoutedModelService = ms
					decisions[len(decisions)-1].Outcome = domain.AttemptSuccess
					handler := wrapWithModerator(outcome.Translator.NewResponseHandler(), rc.Ctx)
					responseStarted = true
					// 注入 rc.StartTime 让 Forward 计算 TTFT（docs/05 §4）
					fwdCtx := upstream.WithRequestStartTime(ctx, rc.StartTime)
					fwd := cfg.sender.Forward(fwdCtx, c.Writer, ep, outcome.Response, handler)
					rc.Usage = fwd.Usage
					if rc.Usage != nil && fwd.TTFTMs > 0 {
						rc.Usage.Meta.TTFTMs = fwd.TTFTMs
					}
					if fwd.FeedErr != nil {
						rc.Error = &domain.AdapterError{
							Class:   domain.ErrTransient,
							Code:    domain.ErrCodeUpstreamError,
							Message: "stream: " + fwd.FeedErr.Error(),
						}
					}
					// endpoint TPM 后扣（docs/04 §10）
					chargeEndpointTPM(ctx, cfg.rateStore, ep, rc.Usage)
					return
				}

				// 失败：排除该 ep，继续 attempt loop
				excluded[ep.ID] = struct{}{}
				decisions[len(decisions)-1].Outcome = domain.AttemptFallback

				// invalid 不属于"换 ep 能解决"的错——上面已处理；这里不会到（safety net）
				if !outcome.Class.IsRetryable() {
					if !responseStarted {
						abortWithCode(c, 502, domain.ErrTransient, domain.ErrCodeUpstreamError,
							"upstream non-retryable: "+outcome.Reason)
					}
					return
				}
			}
		}

		// 所有 model 都耗尽
		if !responseStarted {
			if len(decisions) > 0 {
				decisions[len(decisions)-1].Outcome = domain.AttemptFail
			}
			abortWithDetails(c, 503, domain.ErrTransient, domain.ErrCodeNoEndpointAvailable,
				fmt.Sprintf("no endpoint succeeded after %d attempts", totalAttempts),
				map[string]any{"attempts": totalAttempts},
			)
		}
	}
}

// resolveModel 拿单个 model 的 ModelService + 校验主账号订阅。
//
// 返回 (*ModelService, true, nil) 表示 OK 可以继续；
// (nil, false, nil) 表示该 model 不存在或未订阅，应跳过试下一个 fallback；
// (nil, false, err) 表示依赖故障，调用方应整个 abort。
func resolveModel(
	ctx context.Context,
	catalog ModelCatalog,
	subs SubscriptionChecker,
	accountID, model string,
	msCache map[string]*domain.ModelService,
	subCache map[string]bool,
) (*domain.ModelService, bool, error) {
	ms, cached := msCache[model]
	if !cached {
		var err error
		ms, err = catalog.GetByModel(ctx, model)
		if err != nil {
			return nil, false, err
		}
		msCache[model] = ms
	}
	if ms == nil {
		return nil, false, nil
	}
	if sub, ok := subCache[model]; ok {
		if !sub {
			return nil, false, nil
		}
		return ms, true, nil
	}
	subscribed, err := subs.HasModel(ctx, accountID, ms.ID)
	if err != nil {
		return nil, false, err
	}
	subCache[model] = subscribed
	if !subscribed {
		return nil, false, nil
	}
	return ms, true, nil
}

// =============================================================================
// header 解析
// =============================================================================

// parseMaxAttempts 读 X-Gateway-Max-Attempts header；缺失 / 非法 → 0（用 cfg 默认）。
func parseMaxAttempts(c *gin.Context) int {
	hdr := c.GetHeader(HeaderGatewayMaxAttempts)
	if hdr == "" {
		return 0
	}
	v, err := strconv.Atoi(hdr)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

// parseFallbackModels 读 X-Gateway-Fallback-Models header（逗号分隔，去重保序）；
// docs/03 §5：去重保序，空 model 忽略，数量上限 MaxFallbackModels。
func parseFallbackModels(c *gin.Context) []string {
	hdr := c.GetHeader(HeaderGatewayFallbackModels)
	if hdr == "" {
		return nil
	}
	seen := make(map[string]struct{}, MaxFallbackModels)
	var out []string
	for _, m := range strings.Split(hdr, ",") {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	return out
}

// chargeEndpointTPM 选中 endpoint 响应成功后，把真实 usage.Total 写到 endpoint TPM bucket
//（docs/04 §10）。超限只记 metric；不阻塞响应。
func chargeEndpointTPM(ctx context.Context, store RateLimitStore, ep *domain.Endpoint, usage *domain.Usage) {
	if store == nil || ep == nil || usage == nil || usage.Total <= 0 {
		return
	}
	b := schedule.EndpointTPMChargeBucket(ep, uint32(usage.Total))
	if b == nil {
		return
	}
	// 用 background ctx（响应已完成，客户端 ctx 可能 cancel）
	bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	results, err := store.ChargeBatch(bgCtx, []ratelimit.Bucket{*b})
	if err != nil {
		metric.Inc(metric.RateLimitChargeTotal, "dimension", "tpm", "result", "error")
		return
	}
	metric.Inc(metric.RateLimitChargeTotal, "dimension", "tpm", "result", "ok")
	for _, r := range results {
		if r.Overflow {
			metric.Inc(metric.TPMOverflowTotal, "layer", "endpoint", "dimension", "tpm")
		}
	}
	_ = ctx
}

func routedModelOf(rc *domain.RequestContext) string {
	if rc.RoutedModelService != nil {
		return rc.RoutedModelService.Model
	}
	if rc.ModelService != nil {
		return rc.ModelService.Model
	}
	return ""
}

// =============================================================================
// adapter / translator registry lookup（实现 eligibility.AdapterLookup / TranslatorLookup）
// =============================================================================

type adapterRegistryLookup struct{}

func (adapterRegistryLookup) Has(vendor string) bool {
	return adapter.Get(vendor) != nil
}
func (adapterRegistryLookup) NativeProtocol(vendor string) domain.Protocol {
	f := adapter.Get(vendor)
	if f == nil {
		return domain.ProtoUnknown
	}
	return f.Metadata().NativeProtocol
}
func (adapterRegistryLookup) SupportedModalities(vendor string) []domain.Modality {
	f := adapter.Get(vendor)
	if f == nil {
		return nil
	}
	return f.Metadata().SupportedModalities
}

type translatorRegistryLookup struct{}

func (translatorRegistryLookup) Has(source, target domain.Protocol) bool {
	return translator.Find(source, target) != nil
}

// 旧的 AdaptRepoEndpoints / AdaptRepoCatalog / AdaptRepoSubscriptions 已迁到
// cmd/gateway/middleware_adapters.go（adaptEndpoints / adaptCatalog / adaptSubscriptions）。
// 放在 composition root（cmd/gateway）是为了避免 middleware → ratelimit → repo → middleware
// 的 import cycle。port 由 middleware 拥有；adapter 住装配层。
