package dispatch

import (
	"strconv"
	"time"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// State 给 Policy 看的运行时投影——read-only。
//
// **设计原则**：Policy 是外部可注入实现，State 给接口而非 *state 指针，
// 防止外部 Policy 误改 driver 状态。Dispatcher 内部用 *state 拿到可变态。
//
// **字段语义**：
//
//	Attempts        ── 已完成的 attempt 数（跨 model 累加，从 1 起）
//	AttemptsCap     ── 本请求允许的最大 attempt 数（AttemptCap.Resolve 出来的）
//	Excluded        ── 本请求已尝试过的 endpoint ID 集合（跨 model 累加）
//	CurrentModel    ── 当前 attempt loop 用的 model（fallback 切换会更新）
//	RemainingModels ── ModelChain 里在 CurrentModel 之后还没用到的 model
//	LastVerdict     ── 上一次 Invoker.Invoke 的 Verdict（首次 attempt 前是零值）
type State interface {
	Attempts() int
	AttemptsCap() int
	Excluded() map[int64]struct{}
	CurrentModel() *domain.ModelService
	RemainingModels() []*domain.ModelService
	LastVerdict() Verdict
}

// =============================================================================
// state — Dispatcher 内部的运行时门面，实现 State + 自带 mutator
// =============================================================================
//
// **不持 *RequestContext**：dispatch 跟 RC 解耦后，state 只看 typed Input；
// 副作用（Decision / Usage / RoutedModel / Error）全部写入 s.outcome，
// 由 caller（middleware/schedule.go）从 Outcome 翻译回 RC 字段。

type state struct {
	in Input

	attemptsCap int
	attempts    int
	excluded    map[int64]struct{}

	modelChain []*domain.ModelService
	curIdx     int

	lastVerdict Verdict
	decisions   []domain.Attempt

	startTime time.Time
	outcome   Outcome
}

// newState 用 Input + 已 resolve 的 cap 初始化运行时状态。
func newState(in Input, cap int) *state {
	return &state{
		in:          in,
		attemptsCap: cap,
		excluded:    make(map[int64]struct{}, cap),
		modelChain:  in.ModelChain,
		curIdx:      0,
		decisions:   make([]domain.Attempt, 0, cap),
		startTime:   time.Now(),
	}
}

// =============================================================================
// State interface（read-only，给 Policy 用）
// =============================================================================

func (s *state) Attempts() int                { return s.attempts }
func (s *state) AttemptsCap() int             { return s.attemptsCap }
func (s *state) Excluded() map[int64]struct{} { return s.excluded }

func (s *state) CurrentModel() *domain.ModelService {
	if s.curIdx >= len(s.modelChain) {
		return nil
	}
	return s.modelChain[s.curIdx]
}

func (s *state) RemainingModels() []*domain.ModelService {
	next := s.curIdx + 1
	if next >= len(s.modelChain) {
		return nil
	}
	return s.modelChain[next:]
}

func (s *state) LastVerdict() Verdict { return s.lastVerdict }

// =============================================================================
// Internal mutators（Dispatcher 内部调用）
// =============================================================================

// Exhausted attempt 数已到上限。
func (s *state) Exhausted() bool { return s.attempts >= s.attemptsCap }

// PickQuery 构造给 Selector.Pick 的入参（model / group / exclude）。
func (s *state) PickQuery() PickQuery {
	cur := s.CurrentModel()
	model := ""
	if cur != nil {
		model = cur.Model
	}
	return PickQuery{
		Model:   model,
		Group:   s.in.Identity.Group,
		Exclude: s.excluded,
	}
}

// CurrentModelName 当前轮次的 model 字符串（CandidateSource.ListForModel 入参）；
// 链已耗尽返 ""。
func (s *state) CurrentModelName() string {
	cur := s.CurrentModel()
	if cur == nil {
		return ""
	}
	return cur.Model
}

// Group 用户分组（CandidateSource.ListForModel 入参）。
func (s *state) Group() string { return s.in.Identity.Group }

// Envelope 给 InvokerFactory.For 用（含 RawBytes）+ filterEligible 用。
func (s *state) Envelope() *domain.RequestEnvelope { return s.in.Envelope }

// Handlers 给 dispatcher.step 用——Input 的请求级 Handler 查询端口
// （filterEligible + dispatcher 选 handler 都要）。
func (s *state) Handlers() protocol.Lookup { return s.in.Handlers }

// Record 记一次 attempt：attempts++ / excluded / lastVerdict / decisions append。
// Outcome 字段先填 Unknown，finalize 阶段按终态修正。
//
// **ClassUnknown 不排除**：Unknown = 分类盲区 / 依赖故障（Redis reserve store
// 错等），不是"这个 endpoint 坏了"。跟 cooldown 一样（scheduler.Report 对 Unknown
// 也不 Mark），这里也不把 endpoint 加进 excluded——否则一次 Redis 抖动会把健康
// endpoint 从后续 fallback model 的候选池里永久删掉：endpoint E 同时服务 model
// A/B，chain [A→B]，E 在 A 上撞 store 错被 exclude，fallback 到 B 时 E 是唯一候选
// 却被 excluded 过滤 → eligible 空 → 对健康 endpoint 报 503。attempt 数由
// attemptsCap 兜底，不会因不排除而无限重选同一个 ep。
func (s *state) Record(ep *domain.Endpoint, v Verdict) {
	s.attempts++
	if v.Class != ClassUnknown {
		s.excluded[ep.ID] = struct{}{}
	}
	s.lastVerdict = v

	role := domain.AttemptRolePrimary
	if s.curIdx > 0 {
		role = domain.AttemptRoleFallback
	}

	model := ""
	if cur := s.CurrentModel(); cur != nil {
		model = cur.Model
	}

	s.decisions = append(s.decisions, domain.Attempt{
		Index:       s.attempts,
		Model:       model,
		EndpointID:  strconv.FormatInt(ep.ID, 10),
		AttemptRole: role,
		Outcome:     domain.AttemptUnknown, // finalize 阶段填
		LatencyMs:   v.Latency.Milliseconds(),
		ErrorClass:  v.Class.String(),
		Started:     time.Now().Add(-v.Latency),
	})
}

// SetModel 切换 fallback model。默认按 chain 顺序切；ms 不在 chain 里
// （外部 FallbackPolicy 给了 chain 外选择）时追加进 chain 末尾。
func (s *state) SetModel(ms *domain.ModelService) {
	for i := s.curIdx + 1; i < len(s.modelChain); i++ {
		if s.modelChain[i] == ms {
			s.curIdx = i
			return
		}
	}
	s.modelChain = append(s.modelChain, ms)
	s.curIdx = len(s.modelChain) - 1
}

// ApplyStream Stream 成功后写 RoutedModel + Usage + TTFT + Outcome.Error；
// 全部记到 s.outcome，不直接动 RC（dispatch 跟 RC 解耦）。
func (s *state) ApplyStream(rep StreamReport) {
	routed := s.CurrentModel()
	usage := rep.Usage
	if usage != nil && rep.TTFTMs > 0 {
		usage.Meta.TTFTMs = rep.TTFTMs
	}
	var streamErr *domain.AdapterError
	if rep.Err != nil {
		streamErr = &domain.AdapterError{
			Class:   domain.ErrTransient,
			Code:    domain.ErrCodeUpstreamError,
			Message: "stream: " + rep.Err.Error(),
		}
	}
	s.outcome = Outcome{
		Result:      OutcomeStreamed,
		Usage:       usage,
		StreamErr:   rep.Err,
		TTFTMs:      rep.TTFTMs,
		RoutedModel: routed,
		Error:       streamErr,
	}
	s.finalize()
}

// SetAbort 终止本请求；finalize 写 SchedulingDecision。
func (s *state) SetAbort(a Abort) {
	result := a.Result
	if result == OutcomeUnknown {
		// Policy 没填 Result——按 HTTPCode 兜底（保留兼容；理想是 Policy 都显式填）
		result = inferResultFromHTTPCode(a.HTTPCode)
	}
	s.outcome = Outcome{
		Result:   result,
		HTTPCode: a.HTTPCode,
		Class:    a.Class,
		Reason:   a.Reason,
	}
	s.finalize()
}

// Outcome 取最终产出（含 SchedulingDecision）。
func (s *state) Outcome() Outcome { return s.outcome }

// =============================================================================
// finalize — 终态时给 decisions[].Outcome 补值并写 Outcome.Decision
// =============================================================================

func (s *state) finalize() {
	// attempt 标签只有在有 attempt 时才能补；空 attempts（无候选 / 无 eligible /
	// AttemptCap=0）跳过这一步，Decision 仍然下方填出来。
	if n := len(s.decisions); n > 0 {
		for i := 0; i < n-1; i++ {
			s.decisions[i].Outcome = domain.AttemptFallback
		}
		// 流中断（200 之后 body 转发失败）不算成功——审计上这次 attempt 没有
		// 完整交付；只有干净跑完的 stream 才标 AttemptSuccess。
		if s.outcome.Result == OutcomeStreamed && s.outcome.StreamErr == nil {
			s.decisions[n-1].Outcome = domain.AttemptSuccess
		} else {
			s.decisions[n-1].Outcome = domain.AttemptFail
		}
	}

	primary := s.in.PrimaryModel()
	model := ""
	if primary != nil {
		model = primary.Model
	}
	routedName := ""
	if s.outcome.RoutedModel != nil {
		routedName = s.outcome.RoutedModel.Model
	} else if primary != nil {
		// 没成功路由时审计 routed 兜底成 primary，方便下游 join。
		routedName = primary.Model
	}

	// **永远填 Decision**（即使 attempts 为空）——契约见 Outcome.Decision 注释。
	// 无候选 / 无 eligible / 初始 attempts exhausted 这类调度失败也有审计结构，
	// 下游审计 / log / metric 不需要对 nil Decision 特判。
	s.outcome.Decision = &domain.SchedulingDecision{
		Model:       model,
		RoutedModel: routedName,
		UserGroup:   s.in.Identity.Group,
		Attempts:    s.decisions, // 可能是 nil/empty slice
		DurationMs:  time.Since(s.startTime).Milliseconds(),
	}
}

// inferResultFromHTTPCode 兜底映射——Policy 未显式填 Result 时用。
func inferResultFromHTTPCode(code int) OutcomeResult {
	switch code {
	case 400:
		return OutcomeInvalid
	case 502:
		return OutcomeTerminal
	case 503:
		return OutcomeNoEndpoint
	default:
		return OutcomeDepFail
	}
}
