package schedule

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// Config 构造默认 Scheduler 的依赖集。
//
// **不持有候选 provider**：Candidates 由 caller 通过 Request.Candidates 传入；
// L3 跨 model fallback 走 Request.LoadFallback 回调。
type Config struct {
	Filters     []Filter        // Filter 链；顺序就是执行顺序；最后一个一般是 selector（如 WeightedRandom）
	Cooldown    CooldownManager // CooldownFilter + Selection.Report 失败时调用
	MaxAttempts int             // 全局尝试上限（含 L1 同 ep 内部重试）；0 → 默认 3
	// MaxPerEndpoint 同 endpoint 最大尝试次数（含首次）。
	//
	// 默认 1 = **无 L1 retry**（向后兼容旧行为：失败立刻换 ep）。
	// 设 2-3 适合上游网络偶发抖动场景；同 ep 重试期间不进 cooldown。
	MaxPerEndpoint int
}

// New 构造默认 Scheduler。
func New(cfg Config) Scheduler {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.MaxPerEndpoint <= 0 {
		cfg.MaxPerEndpoint = 1
	}
	return &defaultScheduler{cfg: cfg}
}

type defaultScheduler struct {
	cfg Config
}

func (s *defaultScheduler) BeginSelection(ctx context.Context, req *Request) (Selection, error) {
	if req == nil || req.Model == "" {
		return nil, errors.New("schedule: request.model required")
	}
	if len(req.Candidates) == 0 {
		return nil, fmt.Errorf("schedule: no endpoint for model=%q group=%q", req.Model, req.Group)
	}

	maxAttempts := s.cfg.MaxAttempts
	if req.MaxAttemptsOverride > 0 && req.MaxAttemptsOverride < maxAttempts {
		// header 只能往**更小**方向覆盖（防客户端写超大值占用上游）
		maxAttempts = req.MaxAttemptsOverride
	}

	return &defaultSelection{
		ctx:          ctx,
		cfg:          s.cfg,
		req:          req,
		allCands:     req.Candidates,
		epAttempts:   make(map[int64]int, len(req.Candidates)),
		maxAttempts:  maxAttempts,
		currentModel: req.Model,
		fallbacks:    append([]string(nil), req.FallbackModels...),
	}, nil
}

// defaultSelection 单请求选路状态机。
//
// **不并发**：M7 driver loop 单 goroutine 顺序使用。
type defaultSelection struct {
	ctx            context.Context
	cfg            Config
	req            *Request
	allCands       []*domain.Endpoint // 当前 model 的候选全集；L3 切 fallback 时整个换
	epAttempts     map[int64]int      // ep.ID → 已 attempt 次数（含 L1 重试）；L3 切 fallback 时清零
	attempts       int                // 全局已 attempt 数（跨 fallback 累加；max_attempts 全局上限）
	maxAttempts    int
	decisions      []Decision
	current        *domain.Endpoint // 上一次 Pick 返回的；Report 校验
	pendingRetryEp *domain.Endpoint // L1 retry 标志：下次 Pick 直接返回它（绕过 filter chain）

	// L3 跨模型 fallback 状态
	currentModel string   // 当前正在 try 的 model（首次 = req.Model）
	fallbacks    []string // 剩余未试的 fallback model 队列（FIFO）
}

// Pick 选下一个候选：
//  1. 已达 max_attempts → nil
//  2. **L1 retry fast-path**：上次 Report 标了 pendingRetryEp → 直接返回（绕过 filter chain）
//  3. 排除已耗尽 L1 配额（epAttempts >= max_per_endpoint）的 ep → 跑 filter chain
//  4. chain 返回非空 → 取第一个
//  5. **L3 fallback**：avail 空 / chain 空时切下一个 fallback model 重 list candidates 再 try
//     （attempts 计数继续累加；max_attempts 仍是全局上限）
func (s *defaultSelection) Pick() *domain.Endpoint {
	if s.attempts >= s.maxAttempts {
		return nil
	}

	// L1 retry path
	if s.pendingRetryEp != nil {
		ep := s.pendingRetryEp
		s.pendingRetryEp = nil
		s.epAttempts[ep.ID]++
		s.attempts++
		s.current = ep
		return ep
	}

	for {
		// 排除已耗尽 L1 配额的 ep
		avail := make([]*domain.Endpoint, 0, len(s.allCands))
		for _, ep := range s.allCands {
			if s.epAttempts[ep.ID] >= s.cfg.MaxPerEndpoint {
				continue
			}
			avail = append(avail, ep)
		}

		var picked []*domain.Endpoint
		if len(avail) > 0 {
			// 跑 filter chain（包含 cooldown / limit_read / weighted_random 等）
			picked = runChain(s.ctx, s.cfg.Filters, avail, s.req)
		}

		if len(picked) > 0 {
			// 取第一个（如果链尾是 selector，应该只有 1 个；否则 fallback 取 head）
			ep := picked[0]
			s.epAttempts[ep.ID]++
			s.attempts++
			s.current = ep
			return ep
		}

		// 当前 model 跑空了 → 尝试切 fallback model
		if !s.advanceFallback() {
			return nil
		}
		// loop 继续：用新 model 的 allCands 重跑筛选
	}
}

// advanceFallback 切到下一个 fallback model：拿新 candidates，清 epAttempts。
//
// 返回 true = 切成功（继续 try）；false = fallback 用完 / 没 fallback / 没回调 /
// 拿不到候选。
//
// **失败容错**：req.LoadFallback 拿候选拿不到（DB 错 / 该 model 没 endpoint）
// 跳过这个 fallback，接着试下一个；全部跳完返 false。
//
// req.LoadFallback == nil 时直接返 false（L3 关闭）。
func (s *defaultSelection) advanceFallback() bool {
	if s.req.LoadFallback == nil {
		return false
	}
	for len(s.fallbacks) > 0 {
		next := s.fallbacks[0]
		s.fallbacks = s.fallbacks[1:]
		if next == "" || next == s.currentModel {
			continue
		}
		cands, err := s.req.LoadFallback(s.ctx, next, s.req.Group)
		if err != nil || len(cands) == 0 {
			continue
		}
		s.currentModel = next
		s.allCands = cands
		s.epAttempts = make(map[int64]int, len(cands))
		s.pendingRetryEp = nil
		return true
	}
	return false
}

// Report 记录本次调用结果。
//
// 路由规则：
//   - Success / Invalid    → 不重试不冷却
//   - Transient + L1 配额未耗尽 → 标 pendingRetryEp，下次 Pick 同 ep 重试，**不冷却**
//     （cooldown 会让其它请求误伤该 ep；网络抖动一般马上恢复）
//   - 其它 retryable（Capacity/Permanent/Unknown 或 L1 耗尽的 Transient）→ cooldown + 让 Pick 找下一个
func (s *defaultSelection) Report(ep *domain.Endpoint, result Result) {
	if ep == nil {
		return
	}
	s.decisions = append(s.decisions, Decision{
		AttemptNum: s.attempts,
		EndpointID: ep.ID,
		Vendor:     ep.Vendor,
		Result:     result,
	})

	// Success / Invalid 不进 cooldown / 不重试
	if !result.Class.IsRetryable() {
		return
	}

	// L1 retry：仅 Transient 值得在同 ep 再来一次（429 同 ep 不会突然有额度；
	// Permanent 是配置错；Unknown 保守归 next ep）
	if result.Class == ClassTransient && s.epAttempts[ep.ID] < s.cfg.MaxPerEndpoint {
		s.pendingRetryEp = ep
		return
	}

	// L1 配额耗尽 / 非 Transient 失败 → cooldown 隔离，让 Pick 找下一个候选
	if s.cfg.Cooldown != nil {
		// best-effort：失败仅 log（在 manager 内部），不阻塞 M7
		_ = s.cfg.Cooldown.Mark(s.ctx, ep.ID, result.Class)
	}
}

func (s *defaultSelection) Decisions() []Decision {
	out := make([]Decision, len(s.decisions))
	copy(out, s.decisions)
	return out
}

func (s *defaultSelection) Done() {
	// no-op v0.5；保留扩展位（未来可能写 metric / 释放资源）
	_ = time.Now() // 占位避免未用导入
}
