package selector

import (
	"context"
	"errors"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// Config 构造默认 Scheduler 的依赖。
type Config struct {
	Filters  []Filter           // hard filters（cooldown / limit_read / ...）按顺序执行
	Scorer   Scorer             // Runtime Scoring（docs/03 §8）；nil = 不打分（保留静态 weight）
	Picker Picker             // 最终选择器；nil = 默认 WeightedRandomPicker
	Cooldown CooldownManager    // Report 失败时调用；nil = 不冷却
	Stats    EndpointStatsStore // 统计读模型；nil = 不更新（Report 不写）
}

// New 构造默认 Scheduler。Selector 缺省 = WeightedRandomPicker。
func New(cfg Config) Scheduler {
	if cfg.Picker == nil {
		cfg.Picker = NewWeightedRandomPicker()
	}
	return &defaultScheduler{cfg: cfg}
}

// defaultScheduler 无状态 Pick / Report 实现。
type defaultScheduler struct {
	cfg Config
}

// Pick 跑 Filter 链 → Scorer 调权 → 取第一个候选（链尾 selector 必须缩到 1 个）。
//
// 流程（docs/03 §7 §8）：
//
//  1. 按 req.ExcludeIDs 过滤（已经试过的 ep）
//  2. 跑 hard filters：cooldown / limit_read / ...
//  3. Scorer.Score 调权（runtime scoring，可选）
//  4. 链尾 selector（weighted_random）按 EffectiveWeight 选 1 个
func (s *defaultScheduler) Pick(ctx context.Context, req *Request) (*domain.Endpoint, error) {
	if req == nil {
		return nil, errors.New("schedule: nil request")
	}
	if len(req.Candidates) == 0 {
		return nil, nil
	}

	// 1. 排除已尝试 endpoint
	avail := make([]Candidate, 0, len(req.Candidates))
	for _, c := range req.Candidates {
		if c.Endpoint == nil {
			continue
		}
		if _, excluded := req.ExcludeIDs[c.Endpoint.ID]; excluded {
			continue
		}
		avail = append(avail, c)
	}
	if len(avail) == 0 {
		return nil, nil
	}

	// 2. 跑 filter 链（filter 操作的是 *domain.Endpoint；scorer 操作 Candidate）
	eps := make([]*domain.Endpoint, len(avail))
	for i, c := range avail {
		eps[i] = c.Endpoint
	}
	eps = runChain(ctx, s.cfg.Filters, eps, req)
	if len(eps) == 0 {
		return nil, nil
	}

	// 3. Scorer 调权（可选）：把 filter 剩余 eps 映射回 Candidate（保留原 EffectiveWeight）
	survived := make([]Candidate, 0, len(eps))
	keepSet := make(map[int64]float64, len(avail))
	for _, c := range avail {
		keepSet[c.Endpoint.ID] = c.EffectiveWeight
	}
	for _, ep := range eps {
		survived = append(survived, Candidate{
			Endpoint:        ep,
			EffectiveWeight: keepSet[ep.ID],
		})
	}
	if s.cfg.Scorer != nil {
		survived = s.cfg.Scorer.Score(ctx, survived, req)
	}

	// 4. 用 Selector 按 EffectiveWeight 选 1 个
	chosen := s.cfg.Picker.Select(ctx, survived)
	if chosen == nil {
		return nil, nil
	}
	return chosen.Endpoint, nil
}

// Report 反馈 Send 结果给 cooldown + stats store + metric。
//
// 不决定控制流——dispatch.RetryPolicy.Decide 看 result.Class.IsRetryable 决定继续 / 停止。
//
// **路由**：
//   - Success / Invalid → 不冷却（无价值；Invalid 是客户端错误，cooldown 会误伤其它请求）
//   - Unknown → 不冷却（分类盲区 / 依赖故障，不能把"Redis 抖动"误标成"endpoint 坏了"）
//   - Capacity / Permanent / Transient → cooldown（best-effort，失败不阻塞）
//
// Stats（如配置）：每次 Report 都写一份观测数据（latency / class），供下次 Pick
// 的 Scorer 读取做 Runtime Scoring。
func (s *defaultScheduler) Report(ctx context.Context, ep *domain.Endpoint, result Result) {
	if ep == nil {
		return
	}

	// 写 stats store（runtime scoring 的输入；best-effort）
	if s.cfg.Stats != nil {
		s.cfg.Stats.Record(ctx, ep.ID, result)
	}

	// 失败 + retryable → cooldown
	if result.Class.IsRetryable() && result.Class != ClassUnknown && s.cfg.Cooldown != nil {
		_ = s.cfg.Cooldown.Mark(ctx, ep.ID, result.Class)
	}
}
