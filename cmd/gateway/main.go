// Command llm-gateway is the data plane: accepts LLM client requests, runs them
// through the 10-middleware chain, and forwards them upstream.
//
// Usage (minimal quickstart):
//
//	go run ./cmd/gateway -config ./configs/local/gateway.yaml
//
// See configs/local/gateway.yaml for gateway.yaml; it has four sections —
// server / middleware / database / outbox (apikeys has moved to the DB, so
// there's no more paths.apikeys).
//
// Routing and middleware assembly live in pkg/router; for the DB
// (model_services / endpoints / api_keys), gateway runs infra.Migrate itself at
// startup to create tables. Business data (model_services / endpoints /
// api_keys / pricing, etc.) is maintained by the deployer via direct SQL inserts
// (this repo ships no control plane).
//
// Lifecycle (infra Open + signal handling + reverse-order close) is handled by
// pkg/server; this file only does config loading + business wiring + handing
// the engine off to server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/zereker/llm-gateway/pkg/cachebus"
	"github.com/zereker/llm-gateway/pkg/config"
	"github.com/zereker/llm-gateway/pkg/contentlog"
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/embed"
	"github.com/zereker/llm-gateway/pkg/health"
	"github.com/zereker/llm-gateway/pkg/infra"
	"github.com/zereker/llm-gateway/pkg/invoker"
	"github.com/zereker/llm-gateway/pkg/middleware"
	"github.com/zereker/llm-gateway/pkg/moderation"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
	"github.com/zereker/llm-gateway/pkg/repo"
	"github.com/zereker/llm-gateway/pkg/respcache"
	"github.com/zereker/llm-gateway/pkg/router"
	"github.com/zereker/llm-gateway/pkg/selector"
	"github.com/zereker/llm-gateway/pkg/server"
	"github.com/zereker/llm-gateway/pkg/trace"
	"github.com/zereker/llm-gateway/pkg/usage"

	// vendor Factory blank imports: init() auto-registers into the protocol vendor registry
	_ "github.com/zereker/llm-gateway/pkg/protocol/anthropic"
	_ "github.com/zereker/llm-gateway/pkg/protocol/azureopenai"
	_ "github.com/zereker/llm-gateway/pkg/protocol/bedrock"
	_ "github.com/zereker/llm-gateway/pkg/protocol/cohere"
	_ "github.com/zereker/llm-gateway/pkg/protocol/gemini"
	_ "github.com/zereker/llm-gateway/pkg/protocol/openai"

	// translator blank imports: init() auto-registers into the translator registry
	_ "github.com/zereker/llm-gateway/pkg/translator/anthropic_openai"
	_ "github.com/zereker/llm-gateway/pkg/translator/identity"
	_ "github.com/zereker/llm-gateway/pkg/translator/openai_anthropic"
	_ "github.com/zereker/llm-gateway/pkg/translator/openai_cohere"
	_ "github.com/zereker/llm-gateway/pkg/translator/openai_gemini"
	_ "github.com/zereker/llm-gateway/pkg/translator/responses_openai"
)

func main() {
	configPath := flag.String("config", "./configs/local/gateway.yaml", "path to gateway YAML config")
	flag.Parse()

	// slog default: wrap the JSON handler with trace.CtxHandler so that all
	// *Context-style calls (slog.InfoContext / ErrorContext, etc.) automatically
	// pull trace_id / span_id / baggage (sub_account_id / request_id, etc.) from
	// ctx and add them to the record.
	slog.SetDefault(slog.New(trace.NewCtxHandler(slog.NewJSONHandler(os.Stderr, nil))))

	if err := run(*configPath); err != nil {
		slog.Error("llm-gateway exit", "err", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	// Load the KEK for encrypting the endpoints.auth column; missing or
	// wrong-length key fails fast.
	if err := repo.SetDataKey(cfg.DataKey); err != nil {
		return fmt.Errorf("load data_key: %w", err)
	}

	engine, srv, err := buildEngine(cfg)
	if err != nil {
		return err
	}

	return srv.Serve(cfg.Server.Addr, engine, cfg.Server.ReadHeaderTimeout, cfg.Server.ShutdownTimeout)
}

// buildEngine constructs the deps and wires up router.NewEngine; it also
// returns *server.Server so the caller can decide whether to Serve
// (production) or Close (tests).
//
// gateway startup sequence: OpenDB → infra.Migrate (idempotent, IF NOT EXISTS)
// → repo.CheckSchema to verify the tables exist. Evolve the schema by editing
// pkg/infra/schema.sql directly; gateway can still start even if the
// model_service / endpoint / api_key tables are empty — requests will then get
// 404 / 503 / 401 from M5 / M7 / M2 respectively.
//
// If any intermediate step fails, a defer Closes whatever infra has already
// been opened, to avoid leaking resources.
func buildEngine(cfg *config.Config) (engine *gin.Engine, srv *server.Server, err error) {
	// Hold the real server in a local variable s: on the error path
	// `return nil, nil, err` would overwrite the named return srv with nil, and
	// if a defer relied on srv it would Close nil → panic, masking the real
	// startup error. The defer only refers to s, so any early return can
	// cleanly Close whatever infra is already open.
	s := server.New(slog.Default())
	srv = s
	defer func() {
		if err != nil {
			s.Close()
		}
	}()

	sqldb, err := srv.OpenDB(cfg.Database)
	if err != nil {
		return nil, nil, fmt.Errorf("infra.Open: %w", err)
	}
	if err = infra.Migrate(context.Background(), sqldb); err != nil {
		return nil, nil, fmt.Errorf("infra.Migrate: %w", err)
	}
	if err = repo.CheckSchema(context.Background(), sqldb); err != nil {
		return nil, nil, err
	}

	// M6 RateLimit requires Redis; ping fail-fast at startup
	rdb, err := srv.OpenRedis(cfg.Redis)
	if err != nil {
		return nil, nil, fmt.Errorf("infra.OpenRedis: %w", err)
	}

	// Prometheus counter for the repo TTL LRU cache (hit / miss / error per
	// table). The 5 cached wrappers share this single metrics instance; no
	// reporting happens when it's nil.
	cacheMetrics := newRepoCacheMetrics()

	apikeyProvider := repo.NewCachedAPIKeyProvider(
		repo.NewSQLAPIKeyProvider(sqldb), 10240, 30*time.Second, cacheMetrics,
	)

	// Fast revocation: subscribe to the control plane's cachebus invalidation
	// channel and evict precisely on apikey invalidation events — this shrinks
	// the "key already revoked but data plane still has it cached" window from
	// ≤30s TTL down to sub-second. Best-effort: a subscribe failure only warns
	// (degrading to TTL-only invalidation) and doesn't block startup.
	if stop, subErr := cachebus.NewSubscriber(rdb, "", func(inv cachebus.Invalidation) {
		if inv.Kind == cachebus.KindAPIKey {
			apikeyProvider.Evict(inv.Key)
		}
	}).Start(context.Background()); subErr != nil {
		slog.Warn("cachebus subscribe failed; falling back to TTL-only invalidation", "err", subErr)
	} else {
		srv.AddCloser("cachebus", func() error { stop(); return nil })
	}

	outbox, err := buildOutbox(srv, cfg.UsageEvents)
	if err != nil {
		return nil, nil, fmt.Errorf("usage outbox: %w", err)
	}

	// Content Log (docs/05 §2 + docs/08 §6). none = not constructed, zero overhead
	contentLogger := buildContentLogger(srv, cfg.ContentLog)

	// Runtime Scoring (docs/03 §8): when disabled, scorer = nil and the
	// scheduler falls back to pure static weight
	stats, scorer := buildScoring(cfg.Scoring, rdb)

	// Sender assembly: the Content Logger hooks into the byte stream via hooks (optional)
	senderOpts := []invoker.Option{}
	if contentLogger != nil {
		senderOpts = append(senderOpts, invoker.WithHooks(contentLogger))
	}
	sender := invoker.New(senderOpts...)

	// In-process TTL LRU cache — the repo's only caching strategy.
	// After the deployer changes a business table via SQL, gateway sees the new
	// value within ≤ TTL (default 30s).
	// Each cached wrapper carries its own metrics → llm_gateway_repo_cache_total{table,result}.
	cacheTTL := 30 * time.Second
	catalog := adaptCatalog(repo.NewCachedModelServiceReader(
		repo.NewSQLModelServiceReader(sqldb), 256, cacheTTL, cacheMetrics,
	))

	subs := adaptSubscriptions(repo.NewCachedSubscriptionProvider(
		repo.NewSQLSubscriptionProvider(sqldb), 10240, cacheTTL, cacheMetrics,
	))

	endpointReader := repo.NewCachedEndpointReader(
		repo.NewSQLEndpointReader(sqldb), 1024, 4096, cacheTTL, cacheMetrics,
	)

	// Startup-time endpoint config scan (docs/00 §3 step 6): protocol typos /
	// unregistered vendor / unreachable translator / metadata URL / quirks
	// compile failures — warn + metric, doesn't block startup.
	scanEndpoints(context.Background(), endpointReader, slog.Default())
	// quota policy has only one caching layer: ratelimit.PolicyCache (caches the
	// **parsed** PolicyRule, TTL 30s). We feed it the SQL provider directly here
	// — we don't stack repo.CachedQuotaPolicyProvider on top, since two 30s
	// layers would mean a policy change takes up to 60s to take effect, and the
	// two layers' distinct miss semantics stacked together would be hard to debug.
	quotaPolicyReader := repo.NewSQLQuotaPolicyProvider(sqldb)
	rateStore := ratelimit.NewRedisStore(rdb)
	cooldown := selector.NewRedisCooldownManager(rdb, selector.CooldownDurations{
		Transient: cfg.Selector.Cooldown.Transient,
		Capacity:  cfg.Selector.Cooldown.Capacity,
		Permanent: cfg.Selector.Cooldown.Permanent,
		Invalid:   cfg.Selector.Cooldown.Invalid,
		Unknown:   cfg.Selector.Cooldown.Unknown,
	})

	// Health Probing (docs/03 §10): the prober isn't started when disabled.
	// With recover_cooldown, a successful probe of a cooling endpoint clears
	// its cooldown early (probe-gated recovery).
	startHealthProber(srv, cfg.Health, healthListerAdapter{p: repo.NewSQLEndpointReader(sqldb)}, stats, cooldown)

	picker, inflight := buildPicker(cfg.Selector.Picker)
	sched := selector.New(selector.Config{
		Filters:  buildSchedulerFilters(cfg.Selector.Filters, rateStore, cooldown),
		Picker:   picker,
		Cooldown: cooldown,
		Scorer:   scorer,
		Stats:    stats,
		Affinity: buildAffinity(cfg.Selector.SessionAffinity, rdb),
		Inflight: inflight,
	})

	// Dispatcher assembly (M7 business orchestration: Selector + Invoker +
	// EndpointQuota + Policy). The implementations live in their own packages;
	// cmd/gateway/dispatch_wiring.go only does the wiring.
	// dispatchTracer shares the same trace.Tracer as the middleware AuditTracer,
	// ensuring the dispatch.request / dispatch.attempt spans and the M10 audit
	// live in the same trace tree.
	dispatchTracer := buildTracer(srv, cfg.Trace)
	dispatcher := buildDispatcher(
		adaptEndpoints(endpointReader),
		sched,
		sender,
		rateStore,
		cfg.Selector.MaxAttempts,
		dispatchTracer,
	)

	engine = router.NewEngine(router.Deps{
		BodyLimit: cfg.Request.BodyLimitBytes,
		Timeout:   cfg.Request.Timeout,

		// M2 Auth
		IdentityProvider: apikeyProvider,

		// M4 Budget
		BudgetGate: buildBudgetGate(cfg.Budget),

		// M5 ModelService
		ModelCatalog:        catalog,
		SubscriptionChecker: subs,

		// M6 Limit
		RateLimitStore: rateStore,
		QuotaPolicies:  ratelimit.NewPolicyCache(quotaPolicyReader, 0),

		// M7 Schedule (Dispatcher orchestration: fallback / retry / streaming live in pkg/dispatch)
		Dispatcher: dispatcher,

		// Response cache middleware (after M6, before M7): exact / semantic / no-op
		Cache:          buildCacheMiddleware(cfg.Cache, rdb),
		EmbeddingCache: buildEmbeddingCache(cfg.Cache, rdb),

		// M8 Moderation
		Moderator: buildModerator(cfg.Moderation),

		// M10 Tracing
		UsageOutbox: outbox,
		AuditTracer: dispatchTracer,

		// /readyz dependency checks (docs/06 §13: readiness checks SQL + Redis
		// reachability; Kafka is not checked — a usage-publish failure shouldn't
		// pull traffic out of rotation)
		Readiness: []router.ReadinessChecker{
			{Name: "mysql", Check: sqldb.PingContext},
			{Name: "redis", Check: func(ctx context.Context) error { return rdb.Ping(ctx).Err() }},
		},
	})

	return engine, srv, nil
}

// buildPicker maps cfg.Selector.Picker to a Picker instance (docs/03 §4).
//
//   - "" / "weighted_random": pure EffectiveWeight-weighted random (default)
//   - "p2c": power-of-two-choices — sample two candidates by weight, take the
//     one with fewer pending calls; returns the Inflight tracker the scheduler
//     must maintain for it
//
// An unrecognized name panics directly (fail-fast; surfaces config errors at startup).
func buildPicker(name string) (selector.Picker, *selector.Inflight) {
	switch name {
	case "", "weighted_random":
		return selector.NewWeightedRandomPicker(), nil
	case "p2c":
		inflight := selector.NewInflight()
		return selector.NewP2CPicker(inflight), inflight
	default:
		panic("unknown selector picker: " + name)
	}
}

// buildSchedulerFilters maps names from cfg.Selector.Filters to Filter instances.
//
// This mapping isn't hardcoded in the schedule package — only cmd knows what
// deps are available (redis client / store / cooldown manager). Add a case
// here when introducing a new filter type.
//
// An unrecognized name panics directly (fail-fast; surfaces config errors at startup).
func buildSchedulerFilters(names []string, store ratelimit.Store, cd selector.CooldownManager) []selector.Filter {
	out := make([]selector.Filter, 0, len(names))
	for _, n := range names {
		switch n {
		case "cooldown":
			out = append(out, selector.NewCooldownFilter(cd))
		case "limit_read":
			out = append(out, selector.NewLimitReadFilter(store))
		case "weighted_random":
			// weighted_random is a Selector, not a Filter; it's configured
			// separately under cfg.Selector, so it's ignored here (kept only for
			// backward compatibility with older yaml lists).
			continue
		case "prefix_cache":
			out = append(out, selector.NewPrefixCacheFilter(0)) // 0 = default vnodes=64
		case "busy":
			out = append(out, selector.NewBusyFilter(0)) // 0 = default threshold=0.85
		default:
			panic("unknown scheduler filter: " + n)
		}
	}
	return out
}

// buildTracer constructs a trace.Tracer according to cfg.Driver.
//
//   - slog: default; local structured logging (log/slog)
//   - otel: OpenTelemetry OTLP gRPC export; registers srv.AddCloser to flush on exit
//
// An unrecognized driver panics directly (fail-fast).
func buildTracer(srv *server.Server, cfg config.TraceConfig) trace.Tracer {
	switch cfg.Driver {
	case "", "slog":
		return trace.NewSlogTracer(slog.Default())
	case "otel":
		tp, err := trace.NewOtelProvider(context.Background(), cfg.ServiceName, cfg.Endpoint)
		if err != nil {
			panic(fmt.Sprintf("init otel tracer: %v", err))
		}
		srv.AddCloser("otel-tracer", func() error {
			return tp.Shutdown(context.Background())
		})
		return trace.NewOtelTracer(tp)
	default:
		panic("unknown trace driver: " + cfg.Driver)
	}
}

// buildScoring constructs the Runtime Scoring stats store + scorer (docs/03 §8).
//
// Returns (nil, nil) when disabled — the scheduler then falls back to pure
// static weight, with no runtime scoring at all.
//
// **driver** (cfg.Driver, fail-fast per P5):
//   - inmemory (default): in-process EMA, accumulated independently per
//     replica; use for a single replica or when cross-replica variance is tolerable.
//   - redis: replicas share the same per-endpoint EMA for consistent scoring;
//     use for production multi-replica deployments.
//   - unknown driver → panic (surfaces config errors).
func buildScoring(cfg config.ScoringConfig, rdb *redis.Client) (selector.EndpointStatsStore, selector.Scorer) {
	if !cfg.Enabled {
		return nil, nil
	}
	decay := cfg.EMADecay
	if decay <= 0 {
		decay = 0.2
	}
	var store selector.EndpointStatsStore
	switch cfg.Driver {
	case "", "inmemory":
		store = selector.NewInMemoryStatsStore(decay)
	case "redis":
		store = selector.NewRedisStatsStore(rdb, "llm-gateway:sched", decay, cfg.StatsTTL)
	default:
		panic("buildScoring: unknown scoring.driver " + cfg.Driver + " (want inmemory|redis)")
	}
	baselineMs := float64(200)
	if cfg.LatencyBaseline > 0 {
		baselineMs = float64(cfg.LatencyBaseline.Milliseconds())
	}
	scorer := selector.NewDefaultScorer(store, cfg.MinSamples, baselineMs)
	return store, scorer
}

// buildCacheMiddleware assembles the response cache middleware (after M6, before M7).
//
//   - semantic.enabled → semantic cache (embed the prompt + cosine hit, replaces the exact cache)
//   - enabled          → exact cache (SHA256 body hit)
//   - both off         → no-op (chain unchanged)
//
// Both are Redis-backed (shared across replicas). If the semantic cache's
// embedder isn't configured properly, startup fails fast.
func buildCacheMiddleware(cfg config.CacheConfig, rdb *redis.Client) gin.HandlerFunc {
	switch {
	case cfg.Semantic.Enabled:
		embedder := buildEmbedder(cfg.Semantic.Embedder)
		store := respcache.NewRedisSemanticStore(rdb, "llm-gateway:respcache", cfg.Semantic.MaxEntries)
		threshold := cfg.Semantic.Threshold
		if threshold <= 0 {
			threshold = 0.9
		}
		return middleware.SemanticCache(store, embedder, threshold, cfg.TTL)
	case cfg.Enabled:
		return middleware.ResponseCache(respcache.NewRedisStore(rdb, "llm-gateway:respcache"), cfg.TTL)
	default:
		return func(c *gin.Context) { c.Next() } // no-op
	}
}

// buildEmbeddingCache assembles the cache dedicated to the embedding
// modality — **exact-match only**.
//
// Embeddings are inherently deterministic (no sampling), so an exact cache is
// pure win (RAG workloads re-embedding the same batch of text repeatedly get
// good hit rates); a semantic cache makes no sense for embeddings (they must
// match exactly). So this doesn't reuse buildCacheMiddleware (which may be
// semantic) — whichever cache switch is on, embeddings get the exact cache,
// sharing the same Redis store. cacheKey already folds in modality, so chat and
// embeddings won't collide on key even with an identical body.
func buildEmbeddingCache(cfg config.CacheConfig, rdb *redis.Client) gin.HandlerFunc {
	if cfg.Enabled || cfg.Semantic.Enabled {
		return middleware.ResponseCache(respcache.NewRedisStore(rdb, "llm-gateway:respcache"), cfg.TTL)
	}
	return func(c *gin.Context) { c.Next() } // no-op
}

// buildEmbedder assembles the text embedding backend (P5 fail-fast: unknown driver panics).
func buildEmbedder(cfg config.EmbedderConfig) embed.Embedder {
	switch cfg.Driver {
	case "openai":
		if cfg.APIKey == "" {
			panic("cache.semantic.embedder.driver=openai requires api_key")
		}
		return embed.NewOpenAIEmbedder(cfg.APIKey, cfg.BaseURL, cfg.Model)
	default:
		panic("cache.semantic enabled but embedder.driver unknown: " + cfg.Driver + " (want openai)")
	}
}

// buildAffinity constructs the session affinity store (Redis-backed, shared across replicas).
//
// Returns nil when disabled — the scheduler won't stick sessions. When
// enabled, a client sending the X-Gateway-Session header gets that session
// pinned to the same endpoint (soft affinity: automatically re-selects if the
// pinned endpoint is cooled down / excluded).
func buildAffinity(cfg config.SessionAffinityConfig, rdb *redis.Client) selector.AffinityStore {
	if !cfg.Enabled {
		return nil
	}
	return selector.NewRedisAffinityStore(rdb, "llm-gateway:sched", cfg.TTL)
}

// healthListerAdapter adapts repo.EndpointReader to health.EndpointLister (List returns domain.Endpoint).
type healthListerAdapter struct{ p repo.EndpointReader }

func (a healthListerAdapter) List(ctx context.Context) ([]*domain.Endpoint, error) {
	rows, err := a.p.List(ctx)
	if err != nil {
		return nil, err
	}
	return repo.ToDomainEndpoints(rows), nil
}

// startHealthProber starts the Health Prober according to cfg (docs/03 §10).
//
// Does nothing when disabled. Also skipped when stats == nil (no point
// probing if nothing consumes the results). With cfg.RecoverCooldown, the
// prober also gets the CooldownManager so a successful probe of a cooling
// endpoint releases it early (probe-gated recovery).
func startHealthProber(srv *server.Server, cfg config.HealthConfig, lister health.EndpointLister, stats selector.EndpointStatsStore, cooldown selector.CooldownManager) {
	if !cfg.Enabled || stats == nil {
		return
	}
	var recover selector.CooldownManager
	if cfg.RecoverCooldown {
		recover = cooldown
	}
	prober := health.New(health.Config{
		Source:     health.FilteredSource{Lister: lister},
		Stats:      stats,
		Cooldown:   recover,
		Interval:   cfg.Interval,
		Timeout:    cfg.Timeout,
		Concurrent: cfg.Concurrent,
	})
	ctx, cancel := context.WithCancel(context.Background())
	prober.Run(ctx)
	srv.AddCloser("health-prober", func() error {
		cancel()
		prober.Stop()
		return nil
	})
}

// buildContentLogger constructs a ContentLogger according to cfg.Driver
// (returns nil = disabled).
//
//   - none/"":  returns nil, zero overhead (no hooks attached)
//   - file:     JSONL-appends to a local file; downstream fan-out (S3 / Loki /
//     Kafka content-safety / training-data replay) is left to fluent-bit /
//     vector — gateway doesn't embed a Kafka producer. See
//     docs/architecture/05-metering-billing.md §2 for the rationale.
//
// An unrecognized driver panics directly (surfaces config errors at startup).
func buildContentLogger(srv *server.Server, cfg config.ContentLogConfig) *contentlog.Logger {
	var pub contentlog.Publisher
	switch cfg.Driver {
	case "", "none":
		return nil
	case "file":
		fp, err := contentlog.NewFilePublisher(cfg.File.Path)
		if err != nil {
			panic(fmt.Sprintf("content_log: open file %s: %v", cfg.File.Path, err))
		}
		srv.AddCloser("content-log-file", fp.Close)
		pub = fp
	default:
		panic("unknown content_log driver: " + cfg.Driver)
	}

	bp := contentlog.BackpressureDropOldest
	switch cfg.Backpressure {
	case "drop_newest":
		bp = contentlog.BackpressureDropNewest
	case "block":
		bp = contentlog.BackpressureBlock
	}
	logger := contentlog.New(contentlog.Config{
		Publisher:    pub,
		SampleRate:   cfg.SampleRate,
		MaxBodyBytes: cfg.MaxBodyBytes,
		BufferSize:   cfg.BufferSize,
		Backpressure: bp,
		BlockTimeout: cfg.BlockTimeout,
	})
	srv.AddCloser("content-log-logger", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return logger.Close(ctx)
	})
	return logger
}

// buildBudgetGate constructs a BudgetGate according to cfg.Driver.
//
//   - alwayspass: always allow (default; dev / no billing system)
//   - inmemory:   in-process balance tracking (demo / single primary account;
//     resets to zero on restart since it's in memory)
//
// An unrecognized driver panics directly (surfaces config errors at startup).
func buildBudgetGate(cfg config.BudgetConfig) middleware.BudgetGate {
	switch cfg.Driver {
	case "", "alwayspass":
		return middleware.AlwaysPassGate{}
	case "inmemory":
		return middleware.NewInMemoryBudgetGate(cfg.DefaultBalance)
	default:
		panic("unknown budget driver: " + cfg.Driver)
	}
}

// buildModerator constructs a Moderator according to cfg.Driver. When it
// returns nil, M8 silently passes through.
//
//   - none:   nil (default; no moderation)
//   - openai: OpenAI moderation API client (requires cfg.APIKey)
//
// buildModerator assembles the guardrail chain (Chain itself is a Moderator,
// so it slots into M8 with zero changes there).
//
// Combines the driver's moderator (openai) with an optional denylist guard:
//   - 0 guards  → nil (M8 pass-through)
//   - 1 guard   → return it directly (saves the Chain overhead)
//   - ≥2 guards → Chain runs them in order
func buildModerator(cfg config.ModerationConfig) middleware.Moderator {
	var guards []moderation.NamedGuard

	switch cfg.Driver {
	case "", "none":
		// no driver-side moderator
	case "openai":
		if cfg.APIKey == "" {
			panic("moderation.driver=openai requires moderation.api_key")
		}
		guards = append(guards, moderation.NamedGuard{
			Name: "openai", Guard: middleware.NewOpenAIModerator(cfg.APIKey, cfg.BaseURL),
		})
	default:
		panic("unknown moderation driver: " + cfg.Driver)
	}

	if len(cfg.Denylist.Patterns) > 0 {
		g, err := moderation.NewDenylistGuard(cfg.Denylist.Patterns, cfg.Denylist.CheckOutput)
		if err != nil {
			panic("moderation.denylist: " + err.Error()) // startup fail-fast: bad regex
		}
		guards = append(guards, moderation.NamedGuard{Name: "denylist", Guard: g})
	}

	switch len(guards) {
	case 0:
		return nil
	case 1:
		return guards[0].Guard
	default:
		return moderation.NewChain(guards...)
	}
}

// buildOutbox constructs an OutboxPublisher according to cfg.Driver.
//
// Registers close with srv:
//   - file: closes the file handle
//   - kafka: producer close is auto-registered by srv.NewKafkaProducer;
//     KafkaOutbox shares the same producer reference and doesn't add its own
//     AddCloser (to avoid closing it twice).
func buildOutbox(srv *server.Server, cfg config.UsageEventsConfig) (usage.OutboxPublisher, error) {
	switch cfg.Driver {
	case "file":
		ob, err := usage.NewFileOutbox(cfg.File.Path)
		if err != nil {
			return nil, err
		}
		srv.AddCloser("file-outbox", ob.Close)
		return ob, nil
	case "kafka":
		producer, err := srv.NewKafkaProducer(cfg.Kafka.KafkaConfig)
		if err != nil {
			return nil, err
		}
		if cfg.Kafka.Async {
			// A pure async-kafka outbox has NO durable backstop: billing events
			// are lost on buffer-full past the deadline, on Close-drain timeout,
			// and on any process crash before the channel flushes. Only
			// file_and_kafka (file = source of truth) is safe for billing in
			// production. Warn loudly so this footgun is visible at startup.
			if cfg.Kafka.DLQTopic == "" {
				slog.Warn("usage_events: pure async kafka outbox has no durable backstop; " +
					"billing events can be lost on crash/backpressure. Use file_and_kafka in production.")
			}
			ob := usage.NewAsyncKafkaOutbox(producer, cfg.Kafka.Topic, usage.AsyncOptions{
				BufferSize:  cfg.Kafka.BufferSize,
				MaxRetries:  cfg.Kafka.MaxRetries,
				BackoffBase: cfg.Kafka.BackoffBase,
				DLQTopic:    cfg.Kafka.DLQTopic,
				Logger:      slog.Default(),
			})
			srv.AddCloser("async-kafka-outbox", ob.Close)
			return ob, nil
		}
		return usage.NewKafkaOutbox(producer, cfg.Kafka.Topic), nil
	case "file_and_kafka":
		// dual-write: file is the source of truth (sync commit), Kafka is a
		// best-effort async broadcast. Commits still succeed even if the broker
		// is down; an external replay tool reads the file to resend (see docs/05 §5).
		fileSink, err := usage.NewFileOutbox(cfg.File.Path)
		if err != nil {
			return nil, fmt.Errorf("file_and_kafka: file sink: %w", err)
		}
		producer, err := srv.NewKafkaProducer(cfg.Kafka.KafkaConfig)
		if err != nil {
			return nil, fmt.Errorf("file_and_kafka: kafka producer: %w", err)
		}
		// The kafka side always goes through async: in dual-write mode Kafka is
		// best-effort and must not block the file commit
		kafkaSink := usage.NewAsyncKafkaOutbox(producer, cfg.Kafka.Topic, usage.AsyncOptions{
			BufferSize:  cfg.Kafka.BufferSize,
			MaxRetries:  cfg.Kafka.MaxRetries,
			BackoffBase: cfg.Kafka.BackoffBase,
			DLQTopic:    cfg.Kafka.DLQTopic, // optional; file is already the truth, DLQ is just a per-message fallback
			Logger:      slog.Default(),
		})
		srv.AddCloser("dual-kafka-async", kafkaSink.Close)
		ob := usage.NewDualWriteOutbox(fileSink, kafkaSink, slog.Default())
		srv.AddCloser("dual-file-outbox", ob.Close) // only closes file; kafka is managed by the line above
		return ob, nil
	default:
		return nil, fmt.Errorf("unknown usage_events driver %q (want file|kafka|async_kafka|file_and_kafka)", cfg.Driver)
	}
}
