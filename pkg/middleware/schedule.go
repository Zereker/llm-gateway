package middleware

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/repo"
	"github.com/zereker/llm-gateway/pkg/schedule"
	"github.com/zereker/llm-gateway/pkg/upstream"
)

// ScheduleDeps M7 Schedule middleware 的依赖。
//
// **职责拆分**（v0.5+ 重构）：
//
//   - middleware：拉候选 → BeginSelection → driver loop（Pick → Sender.Send → Report）
//     → 流式 forward → 写 rc.SchedulingDecision。
//   - pkg/schedule：选路决策（Filter 链 + Cooldown + L1/L2/L3 retry 编排）。
//   - pkg/upstream：HTTP / adapter / translator / classify / 流式 chunk loop。
type ScheduleDeps struct {
	Endpoints repo.EndpointReader // 拉候选；典型实现 repo.SQLEndpointReader
	Scheduler schedule.Scheduler  // 选路 + cooldown + retry 编排
	Sender    *upstream.Sender    // 上游调用 + 流式 forward；nil 由 main 端装配，不在此兜底
}

// Schedule 是 M7：拉候选 → 选 endpoint → 调 sender → 反馈结果 → 流式回写。
//
// driver loop 模式：
//  1. ListForModel 拿当前 model 候选
//  2. BeginSelection 构造 Selection 状态机（注入 LoadFallback 回调供 L3 fallback 用）
//  3. for { Pick → sender.Send → sel.Report; 成功就 sender.Forward + break }
//  4. Decisions 写 rc.SchedulingDecision 给 M10
//
// 响应已开始写后，rc.Error 仍可设但 M9 无法覆盖（流式架构固有约束）。
func Schedule(deps ScheduleDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		ctx, end := startSpan(rc.Ctx, "ai-gateway.schedule")
		defer end()
		rc.Ctx = ctx

		if rc.Envelope == nil || rc.ModelService == nil {
			abort(c, 500, domain.ErrUnknown, "internal: M3/M5 did not run before M7")
			return
		}

		// 拉主 model 候选
		cands, err := deps.Endpoints.ListForModel(rc.Ctx, rc.ModelService.Model, rc.Identity.Group)
		if err != nil {
			abort(c, 503, domain.ErrTransient, "list endpoints: "+err.Error())
			return
		}

		if len(cands) == 0 {
			abort(c, 503, domain.ErrTransient,
				fmt.Sprintf("no endpoint for model=%q group=%q", rc.ModelService.Model, rc.Identity.Group))
			return
		}

		req := &schedule.Request{
			Model:               rc.ModelService.Model,
			Group:               rc.Identity.Group,
			TPMCost:             EnsureTPMEstimate(rc, rc.Envelope.RawBytes),
			MaxAttemptsOverride: parseMaxAttempts(c),
			FallbackModels:      parseFallbackModels(c),
			PrefixKey:           prefixKeyFromBody(rc.Envelope.RawBytes),
			Candidates:          cands,
			LoadFallback:        deps.Endpoints.ListForModel,
		}

		sel, err := deps.Scheduler.BeginSelection(rc.Ctx, req)
		if err != nil {
			abort(c, 503, domain.ErrTransient, err.Error())
			return
		}
		defer sel.Done()

		// driver loop
		responseStarted := false
		for {
			ep := sel.Pick()
			if ep == nil {
				if !responseStarted {
					decisions := sel.Decisions()
					abort(c, 503, domain.ErrTransient,
						fmt.Sprintf("schedule: no endpoint succeeded after %d attempts", len(decisions)))
				}
				break
			}
			rc.Endpoint = ep

			// 选完 ep 后追加该 ep 的 TPM bucket key（M10 调账用）
			if rc.RateLimit != nil {
				rc.RateLimit.TPMBucketKeys = append(rc.RateLimit.TPMBucketKeys,
					schedule.EndpointTPMBucketKeys(ep)...)
			}

			outcome, callErr := deps.Sender.Send(rc.Ctx, ep, rc.Envelope, rc.Envelope.RawBytes)
			sel.Report(ep, outcome.ToScheduleResult())

			// translator.TranslateRequest 失败 → Invalid，同请求换 ep 也会失败：直接 abort
			if errors.Is(callErr, upstream.ErrInvalidRequest) {
				abort(c, 400, domain.ErrInvalid, outcome.Reason)
				break
			}

			if !outcome.Success() {
				continue
			}

			// 成功：流式 forward；moderator wrap 在 ctx 里（M8 注入）
			handler := wrapWithModerator(outcome.Translator.NewResponseHandler(), rc.Ctx)
			responseStarted = true
			fwd := deps.Sender.Forward(rc.Ctx, c.Writer, ep, outcome.Response, handler)
			rc.Usage = fwd.Usage
			if fwd.FeedErr != nil {
				rc.Error = &domain.AdapterError{
					Class:   domain.ErrTransient,
					Message: "translator: " + fwd.FeedErr.Error(),
				}
			}
			break
		}

		if decisions := sel.Decisions(); len(decisions) > 0 {
			rc.SchedulingDecision = &domain.SchedulingDecision{
				Attempts: convertDecisions(decisions),
			}
		}
	}
}

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

// parseFallbackModels 读 X-Gateway-Fallback-Models header（逗号分隔）；缺失 / 全空 → nil。
func parseFallbackModels(c *gin.Context) []string {
	hdr := c.GetHeader(HeaderGatewayFallbackModels)
	if hdr == "" {
		return nil
	}
	var out []string
	for _, m := range strings.Split(hdr, ",") {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out
}

// prefixKeyFromBody 截断到 4KiB 上限，避免大 body 影响哈希计算
// （4KiB 对 PrefixCacheFilter 一致性哈希已足够区分）。
func prefixKeyFromBody(rawBody []byte) []byte {
	const maxLen = 4 * 1024
	if len(rawBody) == 0 {
		return nil
	}
	if len(rawBody) > maxLen {
		return rawBody[:maxLen]
	}
	return rawBody
}

// convertDecisions schedule.Decision → domain.Attempt（trace shape）。
//
// Outcome 推导：Success → success；最后一个失败的 → fail；中间失败的 → fallback。
func convertDecisions(ds []schedule.Decision) []domain.Attempt {
	out := make([]domain.Attempt, len(ds))
	for i, d := range ds {
		outcome := domain.AttemptFallback
		if d.Result.Class == schedule.ClassSuccess {
			outcome = domain.AttemptSuccess
		} else if i == len(ds)-1 {
			outcome = domain.AttemptFail
		}
		out[i] = domain.Attempt{
			Index:      d.AttemptNum,
			EndpointID: strconv.FormatInt(d.EndpointID, 10),
			Outcome:    outcome,
			LatencyMs:  d.Result.Latency.Milliseconds(),
			ErrorClass: d.Result.Class.String(),
		}
	}
	return out
}
