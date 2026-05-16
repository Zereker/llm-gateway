package schedule

import (
	"context"
	"sync"
	"time"
)

// Scorer Runtime Scoring 接口（docs/architecture/03-endpoint-scheduling.md §8）。
//
// 输入候选（已过 filter），输出调权后的候选；不淘汰候选（只调 EffectiveWeight）。
// Soft 调整，跟 hard filter 互补：
//
//	hard filter（能不能选）→ scorer（更倾向选谁）→ selector（按 weight 选 1）
//
// 实现 MUST be safe for concurrent use（多 gin handler 并发调）。
type Scorer interface {
	Score(ctx context.Context, candidates []Candidate, req *Request) []Candidate
}

// EndpointStatsStore Scheduler 内部读模型：按 endpoint 聚合的 EMA / 滑窗摘要。
//
// 跟 Metrics / Trace 分层（docs/03 §8）：
//   - Metrics：观测输出，丰富标签
//   - EndpointStatsStore：调度内部状态，只保留 per-endpoint 摘要
//
// **写入**：Scheduler.Report 异步 Record；**读取**：Scorer.Score 同步 Snapshot。
//
// 实现 MUST be safe for concurrent use。
type EndpointStatsStore interface {
	Record(ctx context.Context, endpointID int64, result Result)
	Snapshot(ctx context.Context, endpointID int64) EndpointStats
}

// EndpointStats 单个 endpoint 的运行时统计快照。
type EndpointStats struct {
	// Latency EMA / 滑窗平均（ms）
	LatencyMs float64

	// SuccessRate 最近窗口成功率 [0, 1]；新 endpoint 无样本时为 1.0
	SuccessRate float64

	// SampleCount 窗口内采样数；小于阈值时 Scorer 应给中性 factor
	SampleCount uint32

	// Updated 最近一次 Record 时间
	Updated time.Time
}

// =============================================================================
// InMemoryStatsStore：进程内 EMA 实现
// =============================================================================

// InMemoryStatsStore 进程内 EndpointStatsStore 实现。
//
// **算法**：EMA（指数加权移动平均），decay 默认 0.2（每次新数据占 20% 权重）。
// 简单稳定，无需外部存储；多副本部署下每实例独立累积（适合 dev / 单副本 / runtime
// 评分容忍多副本差异的场景）。
//
// **生产多副本一致性需求**：替换成 Redis-backed 实现；接口不变。
type InMemoryStatsStore struct {
	mu    sync.RWMutex
	stats map[int64]*EndpointStats
	decay float64 // EMA decay；0 < decay <= 1
}

// NewInMemoryStatsStore 构造一个进程内 stats store；decay <= 0 用 0.2 默认。
func NewInMemoryStatsStore(decay float64) *InMemoryStatsStore {
	if decay <= 0 || decay > 1 {
		decay = 0.2
	}
	return &InMemoryStatsStore{
		stats: make(map[int64]*EndpointStats),
		decay: decay,
	}
}

// Record EMA 更新单 endpoint 的 latency / success。
func (s *InMemoryStatsStore) Record(_ context.Context, endpointID int64, result Result) {
	if endpointID == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.stats[endpointID]
	if !ok {
		// 第一次：直接取本次值
		st = &EndpointStats{
			LatencyMs:   float64(result.Latency.Milliseconds()),
			SuccessRate: success01(result.Class),
			SampleCount: 1,
			Updated:     time.Now(),
		}
		s.stats[endpointID] = st
		return
	}
	st.LatencyMs = ema(st.LatencyMs, float64(result.Latency.Milliseconds()), s.decay)
	st.SuccessRate = ema(st.SuccessRate, success01(result.Class), s.decay)
	st.SampleCount++
	st.Updated = time.Now()
}

// Snapshot 取单 endpoint 当前快照；没有数据时返回中性快照（SuccessRate=1, SampleCount=0）。
func (s *InMemoryStatsStore) Snapshot(_ context.Context, endpointID int64) EndpointStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if st, ok := s.stats[endpointID]; ok {
		return *st
	}
	return EndpointStats{SuccessRate: 1.0}
}

// ema 标准指数加权移动平均：new_avg = decay * sample + (1-decay) * old_avg。
func ema(old, sample, decay float64) float64 {
	return decay*sample + (1-decay)*old
}

// success01 把 ErrorClass 映射到 0/1（Success=1，其它=0）。
func success01(c ErrorClass) float64 {
	if c == ClassSuccess {
		return 1.0
	}
	return 0.0
}

// =============================================================================
// DefaultScorer：success / latency factor
// =============================================================================

// DefaultScorer Runtime Scoring 第一版实现（docs/03 §8 公式）。
//
//	effective_weight = base_weight * success_factor * latency_factor
//
// 各 factor 上下限 [0.1, 2.0] 防止某指标把权重打爆。
// 缺数据（SampleCount < MinSamples）的 endpoint 给中性 factor=1.0 保留探索流量。
type DefaultScorer struct {
	store      EndpointStatsStore
	minSamples uint32  // 样本数少于此返回中性 factor
	minFactor  float64 // factor 下限（默认 0.1）
	maxFactor  float64 // factor 上限（默认 2.0）

	// latencyBaseline 用来归一 latency 到 factor：
	//   factor = baseline / actual_latency
	// 默认 200ms。同集群所有 endpoint 用一个 baseline；不打算适配 vendor 间数量级差异。
	latencyBaselineMs float64
}

// NewDefaultScorer 构造 scorer；零值参数自动取合理默认。
func NewDefaultScorer(store EndpointStatsStore, minSamples uint32, baselineMs float64) *DefaultScorer {
	if minSamples == 0 {
		minSamples = 5
	}
	if baselineMs <= 0 {
		baselineMs = 200
	}
	return &DefaultScorer{
		store:             store,
		minSamples:        minSamples,
		minFactor:         0.1,
		maxFactor:         2.0,
		latencyBaselineMs: baselineMs,
	}
}

// Score 按 success / latency factor 调每个候选的 EffectiveWeight。
func (s *DefaultScorer) Score(ctx context.Context, candidates []Candidate, _ *Request) []Candidate {
	if s.store == nil {
		return candidates
	}
	out := make([]Candidate, len(candidates))
	for i, c := range candidates {
		out[i] = c
		stats := s.store.Snapshot(ctx, c.Endpoint.ID)
		if stats.SampleCount < s.minSamples {
			continue // 中性 factor，保留 base weight
		}
		successFactor := clampFactor(stats.SuccessRate, s.minFactor, s.maxFactor)
		latencyFactor := clampFactor(s.latencyBaselineMs/maxFloat(stats.LatencyMs, 1), s.minFactor, s.maxFactor)
		out[i].EffectiveWeight = c.EffectiveWeight * successFactor * latencyFactor
	}
	return out
}

func clampFactor(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
