// Package health 主动健康探测（docs/architecture/03-endpoint-scheduling.md §10）。
//
// **职责**：周期性探测自部署 endpoint（capabilities.SelfHosted=true）的健康状态，
// 把探测结果写到 EndpointStatsStore，作为 Runtime Scoring 的 success/latency factor
// 输入之一。
//
// **约束**：
//   - probe 结果**不**替代资格过滤；协议/模态不支持仍然必须被 eligibility 剔除
//   - probe 只影响 stats / scoring，不直接改业务配置
//   - 探测失败不立即把 endpoint 标 cooldown（被动 cooldown 仍然是主路径的真实失败信号）
//
// **设计**：
//
//	Prober.Run() 启动后台 goroutine
//	    │
//	    ├── 每 interval 拉一遍 self-hosted endpoint 全集（通过 EndpointSource）
//	    ├── 并发 probe（每 endpoint 一次 GET，带超时）
//	    └── probe 结果 → StatsStore.Record（同 Scheduler.Report 同一路径）
//
// 详见 docs/architecture/03-endpoint-scheduling.md §10。
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

// EndpointSource 提供"当前要 probe 的 endpoint 集合"。
//
// 典型实现：repo.EndpointReader.List + 过滤 capabilities.SelfHosted=true + HealthProbeEndpoint != ""。
type EndpointSource interface {
	ListProbeTargets(ctx context.Context) ([]*domain.Endpoint, error)
}

// HTTPDoer 抽象 HTTP client（*http.Client 自动满足）。
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Config Prober 装配参数。
type Config struct {
	Source     EndpointSource
	Stats      selector.EndpointStatsStore // 接收 probe 结果；写法跟 Scheduler.Report 同
	Client     HTTPDoer                    // nil = http.DefaultClient
	Interval   time.Duration               // 默认 30s
	Timeout    time.Duration               // 单次 probe 超时；默认 5s
	Concurrent int                         // 并发 probe 上限；默认 8
	Logger     *slog.Logger                // nil = slog.Default
}

// Prober 周期性 probe 子系统。
type Prober struct {
	cfg    Config
	stop   chan struct{}
	wg     sync.WaitGroup
	logger *slog.Logger
}

// New 构造 Prober。Run() 之前不会发起任何探测。
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

// Run 启动后台 probe 循环；返回 nil 后调用 Stop 等待退出。
//
// 立即启动一次 probe，然后按 Interval 周期。
func (p *Prober) Run(ctx context.Context) {
	if p.cfg.Source == nil || p.cfg.Stats == nil {
		p.logger.Warn("health.Prober: missing Source or Stats; not running")
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.cycle(ctx) // 启动期立即一轮
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

// Stop 优雅停止；等当前 cycle 完成后返回。
func (p *Prober) Stop() {
	close(p.stop)
	p.wg.Wait()
}

// cycle 单次 probe 轮：拉 targets → 并发 probe → 写 stats。
func (p *Prober) cycle(parentCtx context.Context) {
	targets, err := p.cfg.Source.ListProbeTargets(parentCtx)
	if err != nil {
		p.logger.WarnContext(parentCtx, "health.Prober: ListProbeTargets failed", "err", err.Error())
		return
	}
	if len(targets) == 0 {
		return
	}

	sem := make(chan struct{}, p.cfg.Concurrent)
	var wg sync.WaitGroup
	for _, ep := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(ep *domain.Endpoint) {
			defer wg.Done()
			defer func() { <-sem }()
			p.probeOne(parentCtx, ep)
		}(ep)
	}
	wg.Wait()
}

// probeOne 单 endpoint：GET 健康端点 → 按响应填 selector.Result → 写 stats。
func (p *Prober) probeOne(parentCtx context.Context, ep *domain.Endpoint) {
	if ep == nil {
		return
	}
	url := ep.Capabilities.HealthProbeEndpoint
	if url == "" {
		return // 没配 probe URL，跳过
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
}

// recordResult 写 stats + 发 metric。
func (p *Prober) recordResult(ep *domain.Endpoint, r selector.Result) {
	p.cfg.Stats.Record(context.Background(), ep.ID, r)
	metric.Inc("llm_gateway_health_probe_total",
		"endpoint_id", strconv.FormatInt(ep.ID, 10),
		"result", r.Class.String(),
	)
}

// classify HTTP status → ErrorClass（跟 upstream classifyHTTPStatus 一致）。
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
// SelfHostedFilter：包一层 EndpointReader.List，只返 self-hosted + HealthProbeEndpoint 配置的
// =============================================================================

// EndpointLister 抽象"返回所有 endpoint"（avoid直接依赖 repo）。
type EndpointLister interface {
	List(ctx context.Context) ([]*domain.Endpoint, error)
}

// FilteredSource 包装 EndpointLister，只返回 self-hosted + 有 HealthProbeEndpoint 的 endpoint。
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
