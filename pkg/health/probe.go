// Package health provides active health probing (docs/architecture/03-endpoint-scheduling.md §10).
//
// **Responsibility**: periodically probes the health of self-hosted endpoints
// (capabilities.SelfHosted=true) and writes the probe results to
// EndpointStatsStore, feeding one of the success/latency factor inputs of
// Runtime Scoring.
//
// **Constraints**:
//   - probe results do **not** replace eligibility filtering; endpoints whose
//     protocol/modality is unsupported must still be excluded by eligibility
//   - probe only affects stats / scoring / cooldown release, it never mutates
//     business config directly
//   - a failed probe does not immediately mark the endpoint as cooldown (passive
//     cooldown remains the real failure signal on the main path)
//   - probe-gated recovery (optional, Config.Cooldown != nil): a **successful**
//     probe of an endpoint that is currently cooling down clears its cooldown
//     early, so recovery is confirmed by a probe instead of spending a user
//     request on it after the TTL expires. This is release-only — the prober
//     never extends or creates cooldowns.
//
// **Design**:
//
//	Prober.Run() starts a background goroutine
//	    │
//	    ├── every interval, pulls the full set of self-hosted endpoints (via EndpointSource)
//	    ├── probes concurrently (one GET per endpoint, with timeout)
//	    └── probe result → StatsStore.Record (same path as Scheduler.Report)
//
// See docs/architecture/03-endpoint-scheduling.md §10 for details.
package health

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/metric"
	"github.com/zereker/llm-gateway/pkg/selector"
)

// EndpointSource provides "the current set of endpoints to probe".
//
// Typical implementation: repo.EndpointReader.List + filter on
// capabilities.SelfHosted=true + HealthProbeEndpoint != "".
type EndpointSource interface {
	ListProbeTargets(ctx context.Context) ([]*domain.Endpoint, error)
}

// HTTPDoer abstracts the HTTP client (*http.Client satisfies it automatically).
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Config holds the Prober assembly parameters.
type Config struct {
	Source     EndpointSource
	Stats      selector.EndpointStatsStore // receives probe results; written the same way as Scheduler.Report
	Cooldown   selector.CooldownManager    // optional; non-nil enables probe-gated cooldown recovery
	Client     HTTPDoer                    // nil = http.DefaultClient
	Interval   time.Duration               // default 30s
	Timeout    time.Duration               // per-probe timeout; default 5s
	Concurrent int                         // max concurrent probes; default 8
	Logger     *slog.Logger                // nil = slog.Default
}

// Prober is the periodic probing subsystem.
type Prober struct {
	cfg    Config
	stop   chan struct{}
	wg     sync.WaitGroup
	logger *slog.Logger
}

// New constructs a Prober. No probing happens until Run() is called.
func New(cfg Config) *Prober {
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.Concurrent <= 0 {
		cfg.Concurrent = 8
	}
	if cfg.Client == nil {
		cfg.Client = http.DefaultClient
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Prober{
		cfg:    cfg,
		stop:   make(chan struct{}),
		logger: logger,
	}
}

// Run starts the background probe loop; call Stop to wait for it to exit.
//
// Runs one probe immediately, then repeats every Interval.
func (p *Prober) Run(ctx context.Context) {
	if p.cfg.Source == nil || p.cfg.Stats == nil {
		p.logger.Warn("health.Prober: missing Source or Stats; not running")
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.cycle(ctx) // run one round immediately at startup
		t := time.NewTicker(p.cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				p.cycle(ctx)
			}
		}
	}()
}

// Stop shuts down gracefully; returns after the current cycle finishes.
func (p *Prober) Stop() {
	close(p.stop)
	p.wg.Wait()
}

// cycle runs a single probe round: fetch targets → probe concurrently → write stats.
func (p *Prober) cycle(parentCtx context.Context) {
	targets, err := p.cfg.Source.ListProbeTargets(parentCtx)
	if err != nil {
		p.logger.WarnContext(parentCtx, "health.Prober: ListProbeTargets failed", "err", err.Error())
		return
	}
	if len(targets) == 0 {
		return
	}

	// probe-gated recovery: snapshot which targets are cooling down before
	// the round, so a successful probe knows whether it should release one.
	cooling := p.coolingSet(parentCtx, targets)

	sem := make(chan struct{}, p.cfg.Concurrent)
	var wg sync.WaitGroup
	for _, ep := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(ep *domain.Endpoint) {
			defer wg.Done()
			defer func() { <-sem }()
			p.probeOne(parentCtx, ep, cooling[ep.ID])
		}(ep)
	}
	wg.Wait()
}

// coolingSet returns which of the targets are currently in cooldown.
// Best-effort: on lookup failure (or no Cooldown configured) it returns an
// empty map, and this round simply performs no early releases.
func (p *Prober) coolingSet(ctx context.Context, targets []*domain.Endpoint) map[int64]bool {
	if p.cfg.Cooldown == nil {
		return nil
	}
	ids := make([]int64, 0, len(targets))
	for _, ep := range targets {
		if ep != nil {
			ids = append(ids, ep.ID)
		}
	}
	cooling, err := p.cfg.Cooldown.InCooldown(ctx, ids)
	if err != nil {
		p.logger.WarnContext(ctx, "health.Prober: InCooldown failed", "err", err.Error())
		return nil
	}
	return cooling
}

// probeOne handles a single endpoint: GET the health endpoint → fill in a
// selector.Result based on the response → write stats; when the endpoint was
// cooling down and the probe succeeds, release the cooldown early.
func (p *Prober) probeOne(parentCtx context.Context, ep *domain.Endpoint, wasCooling bool) {
	if ep == nil {
		return
	}
	url := ep.Capabilities.HealthProbeEndpoint
	if url == "" {
		return // no probe URL configured, skip
	}

	ctx, cancel := context.WithTimeout(parentCtx, p.cfg.Timeout)
	defer cancel()

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		p.recordResult(ep, selector.Result{Class: selector.ClassPermanent, Reason: "build_request: " + err.Error(), Latency: time.Since(start)})
		return
	}
	resp, err := p.cfg.Client.Do(req)
	latency := time.Since(start)
	if err != nil {
		p.recordResult(ep, selector.Result{Class: selector.ClassTransient, Reason: err.Error(), Latency: latency})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	cls := classify(resp.StatusCode)
	p.recordResult(ep, selector.Result{Class: cls, HTTPCode: resp.StatusCode, Latency: latency})

	// probe-gated recovery: a healthy probe of a cooling endpoint releases it
	// back into rotation before the TTL expires.
	if wasCooling && cls == selector.ClassSuccess && p.cfg.Cooldown != nil {
		if cerr := p.cfg.Cooldown.Clear(parentCtx, ep.ID); cerr != nil {
			p.logger.WarnContext(parentCtx, "health.Prober: cooldown clear failed",
				"endpoint_id", ep.ID, "err", cerr.Error())
			return
		}
		metric.Inc("llm_gateway_health_recover_total",
			"endpoint_id", strconv.FormatInt(ep.ID, 10),
		)
	}
}

// recordResult writes stats + emits a metric.
func (p *Prober) recordResult(ep *domain.Endpoint, r selector.Result) {
	p.cfg.Stats.Record(context.Background(), ep.ID, r)
	metric.Inc("llm_gateway_health_probe_total",
		"endpoint_id", strconv.FormatInt(ep.ID, 10),
		"result", r.Class.String(),
	)
}

// classify maps an HTTP status → ErrorClass (consistent with upstream classifyHTTPStatus).
func classify(code int) selector.ErrorClass {
	switch {
	case code >= 200 && code < 300:
		return selector.ClassSuccess
	case code == 401 || code == 403:
		return selector.ClassPermanent
	case code == 429:
		return selector.ClassCapacity
	case code >= 500:
		return selector.ClassTransient
	case code >= 400:
		return selector.ClassInvalid
	default:
		return selector.ClassUnknown
	}
}

// =============================================================================
// SelfHostedFilter: wraps EndpointReader.List, returning only endpoints that
// are self-hosted and have HealthProbeEndpoint configured
// =============================================================================

// EndpointLister abstracts "return all endpoints" (avoids depending on repo directly).
type EndpointLister interface {
	List(ctx context.Context) ([]*domain.Endpoint, error)
}

// FilteredSource wraps an EndpointLister, returning only self-hosted endpoints
// that have HealthProbeEndpoint set.
type FilteredSource struct {
	Lister EndpointLister
}

func (s FilteredSource) ListProbeTargets(ctx context.Context) ([]*domain.Endpoint, error) {
	if s.Lister == nil {
		return nil, errors.New("health.FilteredSource: nil Lister")
	}
	all, err := s.Lister.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Endpoint, 0, len(all))
	for _, ep := range all {
		if ep == nil {
			continue
		}
		if !ep.Capabilities.SelfHosted {
			continue
		}
		if ep.Capabilities.HealthProbeEndpoint == "" {
			continue
		}
		out = append(out, ep)
	}
	return out, nil
}
