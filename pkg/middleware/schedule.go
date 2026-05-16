package middleware

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/adapter"
	"github.com/zereker/llm-gateway/pkg/contentlog"
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/metric"
	"github.com/zereker/llm-gateway/pkg/repo"
	"github.com/zereker/llm-gateway/pkg/schedule"
	"github.com/zereker/llm-gateway/pkg/schedule/eligibility"
	"github.com/zereker/llm-gateway/pkg/translator"
	"github.com/zereker/llm-gateway/pkg/upstream"
)

// MaxFallbackModels X-Gateway-Fallback-Models header 允许的最多 model 数（docs/03 §5）。
const MaxFallbackModels = 3

// EndpointReader M7 用：按 (model, group) 拉候选 endpoints。
//
// 接口是 middleware-owned；repo 提供实现（middleware.AdaptRepoEndpoints 适配）。
type EndpointReader interface {
	ListForModel(ctx context.Context, model, group string) ([]*domain.Endpoint, error)
}

// ScheduleDeps M7 Schedule middleware 的依赖。
//
// 按 docs/03 §1 重构后：
//   - EndpointReader：M7 自己拉候选（per-model）
//   - Catalog / Subscriptions：fallback model 重新做 M5 校验
//   - Scheduler：无状态 Pick + Report
//   - Sender：单次上游调用
//   - MaxAttempts：全局尝试上限（cfg 默认；header 可往更小覆盖）
type ScheduleDeps struct {
	Endpoints     EndpointReader
	Catalog       ModelCatalog
	Subscriptions SubscriptionChecker
	Scheduler     schedule.Scheduler
	Sender        *upstream.Sender
	MaxAttempts   int // 0 = 默认 3
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
func Schedule(deps ScheduleDeps) gin.HandlerFunc {
	maxAttempts := deps.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		ctx, end := startSpan(rc.Ctx, "llm-gateway.schedule")
		defer end()
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
			ms, ok, err := resolveModel(ctx, deps, rc.Identity.AccountID, model, msCache, subCache)
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
			rawCands, err := deps.Endpoints.ListForModel(ctx, model, rc.Identity.Group)
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

			tpmCost := EnsureTPMEstimate(rc, rc.Envelope.RawBytes)

			// 单 model 内 attempt loop
			for totalAttempts < attemptsCap {
				ep, err := deps.Scheduler.Pick(ctx, &schedule.Request{
					Model:      model,
					Group:      rc.Identity.Group,
					TPMCost:    tpmCost,
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

				// 选完 ep 后追加该 ep 的 TPM bucket key（M6 后扣调账用）
				if rc.RateLimit != nil {
					rc.RateLimit.TPMBucketKeys = append(rc.RateLimit.TPMBucketKeys,
						schedule.EndpointTPMBucketKeys(ep)...)
				}

				outcome, callErr := deps.Sender.Send(ctx, ep, rc.Envelope, rc.Envelope.RawBytes)
				deps.Scheduler.Report(ctx, ep, outcome.ToScheduleResult())

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
					fwd := deps.Sender.Forward(fwdCtx, c.Writer, ep, outcome.Response, handler)
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
	deps ScheduleDeps,
	accountID, model string,
	msCache map[string]*domain.ModelService,
	subCache map[string]bool,
) (*domain.ModelService, bool, error) {
	ms, cached := msCache[model]
	if !cached {
		var err error
		ms, err = deps.Catalog.GetByModel(ctx, model)
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
	subscribed, err := deps.Subscriptions.HasModel(ctx, accountID, ms.ID)
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

// =============================================================================
// AdaptRepoEndpoints
// =============================================================================

// AdaptRepoEndpoints 适配 repo.EndpointReader 为 middleware.EndpointReader。
func AdaptRepoEndpoints(p repo.EndpointReader) EndpointReader {
	return repoEndpointAdapter{p: p}
}

type repoEndpointAdapter struct{ p repo.EndpointReader }

func (a repoEndpointAdapter) ListForModel(ctx context.Context, model, group string) ([]*domain.Endpoint, error) {
	return a.p.ListForModel(ctx, model, group)
}
