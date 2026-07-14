package selector

import (
	"context"
	"errors"
	"strconv"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/metric"
)

// Config holds the dependencies for constructing the default Scheduler.
type Config struct {
	Filters  []Filter           // hard filters (cooldown / limit_read / ...), executed in order
	Scorer   Scorer             // Runtime Scoring (docs/03 §8); nil = no scoring (keep static weight)
	Picker   Picker             // final selector; nil = default WeightedRandomPicker
	Cooldown CooldownManager    // called on Report failure; nil = no cooldown
	Stats    EndpointStatsStore // stats read model; nil = not updated (Report doesn't write)
	Affinity AffinityStore      // session affinity; nil = no sticky session
	Inflight *Inflight          // pending-call tracker for the P2C picker; nil = not tracked
}

// New constructs the default Scheduler. Selector defaults to WeightedRandomPicker.
func New(cfg Config) Scheduler {
	if cfg.Picker == nil {
		cfg.Picker = NewWeightedRandomPicker()
	}

	return &defaultScheduler{cfg: cfg}
}

// defaultScheduler is the stateless Pick / Report implementation.
type defaultScheduler struct {
	cfg Config
}

// Pick runs the Filter chain → Scorer adjusts weights → takes the first candidate (the selector at the end of the chain must narrow it down to 1).
//
// Flow (docs/03 §7 §8):
//
//  1. Filter by req.ExcludeIDs (endpoints already tried)
//  2. Run hard filters: cooldown / limit_read / ...
//  3. Scorer.Score adjusts weights (runtime scoring, optional)
//  4. The selector at the end of the chain (weighted_random) picks 1 by EffectiveWeight
func (s *defaultScheduler) Pick(ctx context.Context, req *Request) (*domain.Endpoint, error) {
	if req == nil {
		return nil, errors.New("schedule: nil request")
	}

	if len(req.Candidates) == 0 {
		return nil, nil
	}

	// 1. Exclude already-tried endpoints
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

	// 2. Run the filter chain (filters operate on *domain.Endpoint; the scorer operates on Candidate)
	eps := make([]*domain.Endpoint, len(avail))
	for i, c := range avail {
		eps[i] = c.Endpoint
	}

	eps = runChain(ctx, s.cfg.Filters, eps, req)
	if len(eps) == 0 {
		return nil, nil
	}

	// 3. Scorer adjusts weights (optional): map the surviving eps back to Candidate (keeping the original EffectiveWeight)
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

	// 3.5 Session affinity (soft): if the pinned endpoint is still in survived (healthy +
	//     eligible + not excluded), stick to it (prefix/KV cache hit); otherwise fall through
	//     to normal selection + re-pin. The session key is namespaced by group to avoid
	//     collisions across tenant pools.
	if s.cfg.Affinity != nil && req.SessionKey != "" {
		ak := req.Group + "|" + req.SessionKey
		if id, ok := s.cfg.Affinity.Get(ctx, ak); ok {
			for _, c := range survived {
				// Match by ID *and* EffectiveWeight > 0: a soft-offlined
				// endpoint (admin sets weight=0 to drain it) still survives the
				// filter chain, but the pickers exclude weight<=0 — the sticky
				// path must honor the same rule, or drained sessions never
				// migrate off. Falls through to normal selection + re-pin.
				if c.Endpoint.ID == id && c.EffectiveWeight > 0 {
					s.cfg.Affinity.Set(ctx, ak, id)  // refresh TTL — steady-state hits must also renew, otherwise an active session loses its pin after the TTL expires
					return s.chosen(c.Endpoint), nil // sticky hit
				}
			}
		}
		// pinned endpoint unavailable (or first time) — select normally, then pin.
		chosen := s.cfg.Picker.Select(ctx, survived)
		if chosen == nil {
			return nil, nil
		}

		s.cfg.Affinity.Set(ctx, ak, chosen.Endpoint.ID)

		return s.chosen(chosen.Endpoint), nil
	}

	// 4. Use the Selector to pick 1 by EffectiveWeight
	chosen := s.cfg.Picker.Select(ctx, survived)
	if chosen == nil {
		return nil, nil
	}

	return s.chosen(chosen.Endpoint), nil
}

// chosen marks the picked endpoint as having one more pending call (input
// signal for the P2C picker). Every Pick that returns an endpoint is paired
// with exactly one Release from the dispatcher (via defer), which decrements
// the counter — regardless of how many Reports the attempt produces.
func (s *defaultScheduler) chosen(ep *domain.Endpoint) *domain.Endpoint {
	if s.cfg.Inflight != nil && ep != nil {
		s.cfg.Inflight.Inc(ep.ID)
	}
	if ep != nil {
		metric.Inc(metric.SelectorEndpointSelectedTotal,
			"endpoint_id", strconv.FormatInt(ep.ID, 10),
			"vendor", ep.Vendor,
			"model", ep.Model,
		)
	}

	return ep
}

// Release decrements the P2C pending-call counter for ep. Called exactly once
// per Pick that returned a non-nil endpoint (the dispatcher defers it), so the
// counter stays correct even when an attempt Reports twice (e.g. a
// supplementary StageStream verdict after a success).
func (s *defaultScheduler) Release(_ context.Context, ep *domain.Endpoint) {
	if s.cfg.Inflight != nil && ep != nil {
		s.cfg.Inflight.Dec(ep.ID)
	}
}

// Report feeds the Send result back to cooldown + stats store + metric.
//
// Does not decide control flow — dispatch.RetryPolicy.Decide looks at result.Class.IsRetryable to decide whether to continue or stop.
//
// **Routing**:
//   - Success / Invalid → no cooldown (no value; Invalid is a client error, cooldown would wrongly punish other requests)
//   - Unknown → no cooldown (classification blind spot / dependency failure; can't mistake "Redis jitter" for "endpoint is broken")
//   - Capacity / Permanent / Transient → cooldown (best-effort, failure doesn't block)
//
// Stats (if configured): every Report writes an observation (latency / class), which the next Pick's
// Scorer reads for Runtime Scoring.
func (s *defaultScheduler) Report(ctx context.Context, ep *domain.Endpoint, result Result) {
	if ep == nil {
		return
	}

	// NB: the P2C pending-call counter is NOT decremented here — Report can
	// fire more than once per attempt (a supplementary StageStream verdict
	// after a success), which would double-count. The dispatcher decrements it
	// exactly once via a deferred Release. See Scheduler.Release.

	// write to stats store (input for runtime scoring; best-effort)
	if s.cfg.Stats != nil {
		s.cfg.Stats.Record(ctx, ep.ID, result)
	}

	outcome := "fail"
	if result.Class == ClassSuccess {
		outcome = "success"
	}
	metric.Inc(metric.SelectorEndpointCallTotal,
		"endpoint_id", strconv.FormatInt(ep.ID, 10),
		"vendor", ep.Vendor,
		"model", ep.Model,
		"outcome", outcome,
		"class", result.Class.String(),
	)

	// failure + retryable → cooldown
	if result.Class.IsRetryable() && result.Class != ClassUnknown && s.cfg.Cooldown != nil {
		if err := s.cfg.Cooldown.Mark(ctx, ep.ID, result.Class, result.RetryAfter); err == nil {
			metric.Inc(metric.SelectorCooldownEnterTotal,
				"endpoint_id", strconv.FormatInt(ep.ID, 10),
				"vendor", ep.Vendor,
				"class", result.Class.String(),
			)
		}
	}
}
