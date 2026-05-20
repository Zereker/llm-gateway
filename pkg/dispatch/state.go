package dispatch

import (
	"strconv"
	"time"

	"github.com/zereker/llm-gateway/pkg/domain"
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

type state struct {
	rc *domain.RequestContext

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

// newState 用 rc + 已 resolve 的 cap 初始化运行时状态。
func newState(rc *domain.RequestContext, cap int) *state {
	return &state{
		rc:          rc,
		attemptsCap: cap,
		excluded:    make(map[int64]struct{}, cap),
		modelChain:  rc.ModelChain,
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

// Query 构造给 Selector.Select 的入参。
func (s *state) Query() Query {
	cur := s.CurrentModel()
	model := ""
	if cur != nil {
		model = cur.Model
	}
	return Query{
		Model:    model,
		Envelope: s.rc.Envelope,
		Identity: s.rc.Identity,
		Exclude:  s.excluded,
	}
}

// Envelope 给 InvokerFactory.For 用。
func (s *state) Envelope() *domain.RequestEnvelope { return s.rc.Envelope }

// Body 给 InvokerFactory.For 用——原始请求字节。
func (s *state) Body() []byte {
	if s.rc.Envelope == nil {
		return nil
	}
	return s.rc.Envelope.RawBytes
}

// Record 记一次 attempt：attempts++ / excluded / lastVerdict / decisions append。
// Outcome 字段先填 Unknown，finalize 阶段按终态修正。
func (s *state) Record(ep *domain.Endpoint, v Verdict) {
	s.attempts++
	s.excluded[ep.ID] = struct{}{}
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

// ApplyStream Stream 成功后写 RoutedModel + Usage + TTFT + finalize。
func (s *state) ApplyStream(rep StreamReport) {
	s.rc.RoutedModelService = s.CurrentModel()
	s.rc.Usage = rep.Usage
	if rep.Usage != nil && rep.TTFTMs > 0 {
		s.rc.Usage.Meta.TTFTMs = rep.TTFTMs
	}
	if rep.Err != nil {
		s.rc.Error = &domain.AdapterError{
			Class:   domain.ErrTransient,
			Code:    domain.ErrCodeUpstreamError,
			Message: "stream: " + rep.Err.Error(),
		}
	}
	s.outcome = Outcome{
		Result:    OutcomeStreamed,
		Usage:     rep.Usage,
		StreamErr: rep.Err,
		TTFTMs:    rep.TTFTMs,
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

// Outcome 取最终产出（含 SchedulingDecision 写入到 rc.SchedulingDecision）。
func (s *state) Outcome() Outcome { return s.outcome }

// =============================================================================
// finalize — 终态时给 decisions[].Outcome 补值并写 rc.SchedulingDecision
// =============================================================================

func (s *state) finalize() {
	if len(s.decisions) > 0 {
		last := len(s.decisions) - 1
		for i := 0; i < last; i++ {
			s.decisions[i].Outcome = domain.AttemptFallback
		}
		if s.outcome.Result == OutcomeStreamed {
			s.decisions[last].Outcome = domain.AttemptSuccess
		} else {
			s.decisions[last].Outcome = domain.AttemptFail
		}

		routed := ""
		if s.rc.RoutedModelService != nil {
			routed = s.rc.RoutedModelService.Model
		} else if s.rc.ModelService != nil {
			routed = s.rc.ModelService.Model
		}
		model := ""
		if s.rc.ModelService != nil {
			model = s.rc.ModelService.Model
		}

		s.rc.SchedulingDecision = &domain.SchedulingDecision{
			Model:       model,
			RoutedModel: routed,
			UserGroup:   s.rc.Identity.Group,
			Attempts:    s.decisions,
			DurationMs:  time.Since(s.startTime).Milliseconds(),
		}
		s.outcome.Decision = s.rc.SchedulingDecision
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
