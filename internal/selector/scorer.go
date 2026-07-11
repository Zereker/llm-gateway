package selector

import (
	"context"
	"sync"
	"time"
)

// Scorer is the Runtime Scoring interface (docs/architecture/03-endpoint-scheduling.md §8).
//
// Takes candidates (already filtered) as input, outputs candidates with adjusted weights; it doesn't
// eliminate candidates (only adjusts EffectiveWeight). A soft adjustment, complementary to hard filters:
//
//	hard filter (can it be picked) → scorer (which one is preferred) → selector (pick 1 by weight)
//
// Implementations MUST be safe for concurrent use (called concurrently by multiple gin handlers).
type Scorer interface {
	Score(ctx context.Context, candidates []Candidate, req *Request) []Candidate
}

// EndpointStatsStore is the Scheduler's internal read model: an EMA / sliding-window summary aggregated per endpoint.
//
// Layered separately from Metrics / Trace (docs/03 §8):
//   - Metrics: observability output, richly labeled
//   - EndpointStatsStore: internal scheduling state, keeps only a per-endpoint summary
//
// **Write**: Scheduler.Report Records asynchronously; **Read**: Scorer.Score Snapshots synchronously.
//
// Implementations MUST be safe for concurrent use.
type EndpointStatsStore interface {
	Record(ctx context.Context, endpointID int64, result Result)
	Snapshot(ctx context.Context, endpointID int64) EndpointStats
}

// EndpointStats is the runtime stats snapshot for a single endpoint.
type EndpointStats struct {
	// LatencyMs is the EMA / sliding-window average (ms)
	LatencyMs float64

	// SuccessRate is the recent-window success rate [0, 1]; 1.0 for a new endpoint with no samples
	SuccessRate float64

	// SampleCount is the sample count within the window; Scorer should give a neutral factor below the threshold
	SampleCount uint32

	// Updated is the time of the most recent Record
	Updated time.Time
}

// =============================================================================
// InMemoryStatsStore: in-process EMA implementation
// =============================================================================

// InMemoryStatsStore is an in-process EndpointStatsStore implementation.
//
// **Algorithm**: EMA (exponential moving average), decay defaults to 0.2 (each new data point gets 20% weight).
// Simple and stable, no external storage needed; each instance accumulates independently in a multi-replica
// deployment (suitable for dev / single-replica / scenarios where runtime scoring tolerates cross-replica variance).
//
// **For production multi-replica consistency needs**: swap in the Redis-backed implementation; the interface stays the same.
type InMemoryStatsStore struct {
	mu    sync.RWMutex
	stats map[int64]*EndpointStats
	decay float64 // EMA decay; 0 < decay <= 1
}

// NewInMemoryStatsStore constructs an in-process stats store; decay <= 0 uses the 0.2 default.
func NewInMemoryStatsStore(decay float64) *InMemoryStatsStore {
	if decay <= 0 || decay > 1 {
		decay = 0.2
	}
	return &InMemoryStatsStore{
		stats: make(map[int64]*EndpointStats),
		decay: decay,
	}
}

// Record updates a single endpoint's latency / success via EMA.
func (s *InMemoryStatsStore) Record(_ context.Context, endpointID int64, result Result) {
	if endpointID == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.stats[endpointID]
	if !ok {
		// first time: take this value directly
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

// Snapshot takes the current snapshot for a single endpoint; returns a neutral snapshot (SuccessRate=1, SampleCount=0) when there's no data.
func (s *InMemoryStatsStore) Snapshot(_ context.Context, endpointID int64) EndpointStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if st, ok := s.stats[endpointID]; ok {
		return *st
	}
	return EndpointStats{SuccessRate: 1.0}
}

// ema is the standard exponential moving average: new_avg = decay * sample + (1-decay) * old_avg.
func ema(old, sample, decay float64) float64 {
	return decay*sample + (1-decay)*old
}

// success01 maps ErrorClass to 0/1 (Success=1, everything else=0).
func success01(c ErrorClass) float64 {
	if c == ClassSuccess {
		return 1.0
	}
	return 0.0
}

// =============================================================================
// DefaultScorer: success / latency factor
// =============================================================================

// DefaultScorer is the first-version Runtime Scoring implementation (docs/03 §8 formula).
//
//	effective_weight = base_weight * success_factor * latency_factor
//
// Each factor is bounded to [0.1, 2.0] to prevent any single metric from blowing up the weight.
// Endpoints lacking data (SampleCount < MinSamples) get a neutral factor=1.0 to preserve exploration traffic.
type DefaultScorer struct {
	store      EndpointStatsStore
	minSamples uint32  // return a neutral factor below this sample count
	minFactor  float64 // factor lower bound (default 0.1)
	maxFactor  float64 // factor upper bound (default 2.0)

	// latencyBaselineMs normalizes latency into a factor:
	//   factor = baseline / actual_latency
	// Defaults to 200ms. All endpoints in the same cluster use one baseline; not intended to
	// adapt to order-of-magnitude differences between vendors.
	latencyBaselineMs float64
}

// NewDefaultScorer constructs a scorer; zero-value parameters automatically get sensible defaults.
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

// Score adjusts each candidate's EffectiveWeight by success / latency factor.
func (s *DefaultScorer) Score(ctx context.Context, candidates []Candidate, _ *Request) []Candidate {
	if s.store == nil {
		return candidates
	}
	out := make([]Candidate, len(candidates))
	for i, c := range candidates {
		out[i] = c
		stats := s.store.Snapshot(ctx, c.Endpoint.ID)
		if stats.SampleCount < s.minSamples {
			continue // neutral factor, keep base weight
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
