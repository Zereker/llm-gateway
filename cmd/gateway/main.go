// Command llm-gateway is the data plane: it takes LLM client requests →
// runs the 10-middleware chain → forwards to upstream.
//
// Usage (minimal quickstart):
//
//	go run ./cmd/gateway -config ./configs/local/gateway.yaml
//
// See configs/local/gateway.yaml for gateway.yaml; it has four sections:
// server / middleware / database / outbox (apikeys has migrated to the DB,
// there's no more paths.apikeys).
//
// Routing and middleware wiring live in pkg/router; for the DB
// (model_services / endpoints / api_keys) the gateway runs infra.Migrate
// itself at startup to create tables; business data (model_services /
// endpoints / api_keys / pricing, etc.) is maintained by the deployer via
// direct SQL inserts (this repo ships no control plane).
//
// Lifecycle (infra Open + signal handling + reverse-order close) lives in
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
	"github.com/zereker/llm-gateway/pkg/health"
	"github.com/zereker/llm-gateway/pkg/infra"
	"github.com/zereker/llm-gateway/pkg/middleware"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
	"github.com/zereker/llm-gateway/pkg/repo"
	"github.com/zereker/llm-gateway/pkg/respcache"
	"github.com/zereker/llm-gateway/pkg/router"
	"github.com/zereker/llm-gateway/pkg/selector"
	"github.com/zereker/llm-gateway/pkg/server"
	"github.com/zereker/llm-gateway/pkg/trace"
	"github.com/zereker/llm-gateway/pkg/invoker"
	"github.com/zereker/llm-gateway/pkg/usage"

	// vendor Factory blank imports: init() auto-registers with the protocol vendor registry
	_ "github.com/zereker/llm-gateway/pkg/protocol/anthropic"
	_ "github.com/zereker/llm-gateway/pkg/protocol/gemini"
	_ "github.com/zereker/llm-gateway/pkg/protocol/azureopenai"
	_ "github.com/zereker/llm-gateway/pkg/protocol/openai"

	// translator blank imports: init() auto-registers with the translator registry
	_ "github.com/zereker/llm-gateway/pkg/translator/anthropic_openai"
	_ "github.com/zereker/llm-gateway/pkg/translator/identity"
	_ "github.com/zereker/llm-gateway/pkg/translator/openai_anthropic"
	_ "github.com/zereker/llm-gateway/pkg/translator/openai_gemini"
	_ "github.com/zereker/llm-gateway/pkg/translator/responses_openai"
)

func main() {
	configPath := flag.String("config", "./configs/local/gateway.yaml", "path to gateway YAML config")
	flag.Parse()

	// slog default: wrap the JSON handler with trace.CtxHandler so every
	// *Context-family call (slog.InfoContext / ErrorContext, etc.) automatically
	// pulls trace_id / span_id / baggage (sub_account_id / request_id, etc.)
	// from ctx into the record.
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

	// Load the KEK for encrypting the endpoints.auth column; fail fast if missing or wrong length.
	if err := repo.SetDataKey(cfg.DataKey); err != nil {
		return fmt.Errorf("load data_key: %w", err)
	}

	engine, srv, err := buildEngine(cfg)
	if err != nil {
		return err
	}

	return srv.Serve(cfg.Server.Addr, engine, cfg.Server.ReadHeaderTimeout, cfg.Server.ShutdownTimeout)
}

// buildEngine constructs deps and wires up router.NewEngine; it also returns
// *server.Server so the caller can decide whether to Serve (production) or
// Close (tests).
//
// Gateway startup sequence: OpenDB → infra.Migrate (idempotent IF NOT EXISTS)
// → repo.CheckSchema to verify the tables exist. Schema evolution is done
// directly in pkg/infra/schema.sql; if the tables have no model_service /
// endpoint / api_key rows the gateway still starts, and incoming requests
// will get 404 / 503 / 401 from M5 / M7 / M2 respectively.
//
// If any intermediate step fails, a defer Closes whatever infra is already
// open, to avoid leaking resources.
func buildEngine(cfg *config.Config) (engine *gin.Engine, srv *server.Server, err error) {
	// Hold the real server in a local variable s: on an error path,
	// `return nil, nil, err` overwrites the named return srv with nil, and if
	// the defer relied on srv it would Close nil → panic, masking the real
	// startup error. The defer only ever references s, so any early return
	// can cleanly Close whatever infra is already open.
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

	// M6 RateLimit requires Redis; ping at startup and fail fast
	rdb, err := srv.OpenRedis(cfg.Redis)
	if err != nil {
		return nil, nil, fmt.Errorf("infra.OpenRedis: %w", err)
	}

	// Prometheus counter for the repo TTL LRU cache (hit / miss / error per table).
	// The 5 cached wrappers share this single metrics instance; nil means no reporting.
	cacheMetrics := newRepoCacheMetrics()

	apikeyProvider := repo.NewCachedAPIKeyProvider(
		repo.NewSQLAPIKeyProvider(sqldb), 10240, 30*time.Second, cacheMetrics,
	)

	// Fast revocation: subscribe to the control plane's cachebus invalidation
	// channel and evict precisely on an apikey invalidation—this shrinks the
	// window where "key is revoked but the data plane still has it cached
	// valid" from the ≤30s TTL down to sub-second. Best-effort: a subscribe
	// failure only warns (degrading to TTL-only invalidation) and doesn't
	// block startup.
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

	// Runtime Scoring (docs/03 §8): when disabled, scorer = nil and the scheduler falls back to pure static weight
	stats, scorer := buildScoring(cfg.Scoring, rdb)

	// Health Probing (docs/03 §10): prober is not started when disabled
	startHealthProber(srv, cfg.Health, healthListerAdapter{p: repo.NewSQLEndpointReader(sqldb)}, stats)

	// Sender wiring: the Content Logger hooks into the byte stream (optional)
	senderOpts := []invoker.Option{}
	if contentLogger != nil {
		senderOpts = append(senderOpts, invoker.WithHooks(contentLogger))
	}
	sender := invoker.New(senderOpts...)

	// In-process TTL LRU cache—repo's only caching strategy.
	// After the deployer's SQL edits a business table, the gateway sees the new
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

	// Startup-time endpoint config scan (docs/00 §3 step 6): protocol typo /
	// unregistered vendor / unreachable translator / metadata URL / quirks
	// compile failure—warn + metric, doesn't block startup.
	scanEndpoints(context.Background(), endpointReader, slog.Default())
	// Quota policy only has a single caching layer: ratelimit.PolicyCache
	// (caches the **parsed** PolicyRule, TTL 30s). We feed the SQL provider
	// directly here—no additional repo.CachedQuotaPolicyProvider layered on
	// top, since two 30s layers would mean a policy change takes up to 60s in
	// the worst case to propagate, and the two layers' independent miss
	// semantics compound and make troubleshooting harder.
	quotaPolicyReader := repo.NewSQLQuotaPolicyProvider(sqldb)
	rateStore := ratelimit.NewRedisStore(rdb)
	cooldown := selector.NewRedisCooldownManager(rdb, selector.CooldownDurations{
		Transient: cfg.Selector.Cooldown.Transient,
		Capacity:  cfg.Selector.Cooldown.Capacity,
		Permanent: cfg.Selector.Cooldown.Permanent,
		Invalid:   cfg.Selector.Cooldown.Invalid,
		Unknown:   cfg.Selector.Cooldown.Unknown,
	})
	sched := selector.New(selector.Config{
		Filters:  buildSchedulerFilters(cfg.Selector.Filters, rateStore, cooldown),
		Picker:   selector.NewWeightedRandomPicker(),
		Cooldown: cooldown,
		Scorer:   scorer,
		Stats:    stats,
		Affinity: buildAffinity(cfg.Selector.SessionAffinity, rdb),
	})

	// Dispatcher wiring (M7 business orchestration: Selector + Invoker + EndpointQuota + Policy).
	// The implementations live in their own packages; cmd/gateway/dispatch_wiring.go only does the wiring.
	// dispatchTracer shares the same trace.Tracer as middleware's AuditTracer, ensuring the
	// dispatch.request / dispatch.attempt spans and the M10 audit stay in the same trace tree.
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

		// M7 Schedule (Dispatcher orchestration: fallback / retry / streaming live inside pkg/dispatch)
		Dispatcher: dispatcher,

		// Response cache (after M6, before M7); disabled = nil no-op
		ResponseCache: buildResponseCache(cfg.Cache, rdb),
		CacheTTL:      cfg.Cache.TTL,

		// M8 Moderation
		Moderator: buildModerator(cfg.Moderation),

		// M10 Tracing
		UsageOutbox: outbox,
		AuditTracer: dispatchTracer,

		// /readyz dependency checks (docs/06 §13: readiness checks SQL + Redis
		// reachability; Kafka is not checked—a usage publish failure shouldn't
		// pull traffic out of rotation)
		Readiness: []router.ReadinessChecker{
			{Name: "mysql", Check: sqldb.PingContext},
			{Name: "redis", Check: func(ctx context.Context) error { return rdb.Ping(ctx).Err() }},
		},
	})

	return engine, srv, nil
}

// buildSchedulerFilters maps cfg.Selector.Filters names → Filter instances.
//
// This mapping isn't hardcoded in the schedule pkg—only cmd knows which deps
// exist (redis client / store / cooldown manager). Add a new case here when
// introducing a new filter type.
//
// An unrecognized name panics directly (fail-fast; surfaces a config error at startup).
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
			// separately under cfg.Selector, so it's ignored here (kept only
			// for backward compatibility with older yaml lists).
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

// buildTracer constructs a trace.Tracer based on cfg.Driver.
//
//   - slog: default; local structured logging (log/slog)
//   - otel: OpenTelemetry OTLP gRPC export, registers srv.AddCloser to flush on shutdown
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

// buildScoring constructs the stats store + scorer for Runtime Scoring (docs/03 §8).
//
// Returns (nil, nil) when disabled—the scheduler then falls back to pure
// static weight, with no runtime scoring at all.
//
// **driver** (cfg.Driver, fail-fast per P5):
//   - inmemory (default): in-process EMA, accumulated independently per replica; for single-replica or when tolerating cross-replica variance.
//   - redis: multiple replicas share the same per-endpoint EMA for consistent scoring; for production multi-replica setups.
//   - unknown driver → panic (surfaces a config error).
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

// buildResponseCache constructs the response cache store (Redis-backed, shared across replicas).
// Returns nil when disabled—the ResponseCache middleware becomes a no-op.
func buildResponseCache(cfg config.CacheConfig, rdb *redis.Client) middleware.ResponseCacheStore {
	if !cfg.Enabled {
		return nil
	}
	return respcache.NewRedisStore(rdb, "llm-gateway:respcache")
}

// buildAffinity constructs the session affinity store (Redis-backed, shared across replicas).
//
// Returns nil when disabled—the scheduler won't pin sessions. When enabled, a
// client sending the X-Gateway-Session header pins that session to the same
// endpoint (soft affinity: automatically re-selects if the pinned endpoint
// is put in cooldown/excluded).
func buildAffinity(cfg config.SessionAffinityConfig, rdb *redis.Client) selector.AffinityStore {
	if !cfg.Enabled {
		return nil
	}
	return selector.NewRedisAffinityStore(rdb, "llm-gateway:sched", cfg.TTL)
}

// healthListerAdapter adapts repo.EndpointReader → health.EndpointLister (List returns domain.Endpoint).
type healthListerAdapter struct{ p repo.EndpointReader }

func (a healthListerAdapter) List(ctx context.Context) ([]*domain.Endpoint, error) {
	rows, err := a.p.List(ctx)
	if err != nil {
		return nil, err
	}
	return repo.ToDomainEndpoints(rows), nil
}

// startHealthProber starts the Health Prober per cfg (docs/03 §10).
//
// Does nothing when disabled. Also skipped when stats == nil (probe results
// have no consumer, so there'd be no point).
func startHealthProber(srv *server.Server, cfg config.HealthConfig, lister health.EndpointLister, stats selector.EndpointStatsStore) {
	if !cfg.Enabled || stats == nil {
		return
	}
	prober := health.New(health.Config{
		Source:     health.FilteredSource{Lister: lister},
		Stats:      stats,
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

// buildContentLogger constructs a ContentLogger based on cfg.Driver (returns nil = disabled).
//
//   - none/"":  returns nil, zero overhead (no hooks attached)
//   - file:     JSONL appended to a local file; downstream fan-out (S3 / Loki
//     / Kafka content safety / training-data replay) is delegated to
//     fluent-bit / vector—the gateway doesn't embed a Kafka producer.
//     See docs/architecture/05-metering-billing.md §2 for the rationale.
//
// An unrecognized driver panics directly (surfaces a config error at startup).
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

// buildBudgetGate constructs a BudgetGate based on cfg.Driver.
//
//   - alwayspass: always allows (default; dev / no billing system)
//   - inmemory:   in-process balance tracking (demo / single primary account; resets on restart since it's in memory)
//
// An unrecognized driver panics directly (surfaces a config error at startup).
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

// buildModerator constructs a Moderator based on cfg.Driver. When it returns
// nil, M8 silently passes through.
//
//   - none:   nil (default; no moderation)
//   - openai: OpenAI moderation API client (requires cfg.APIKey)
func buildModerator(cfg config.ModerationConfig) middleware.Moderator {
	switch cfg.Driver {
	case "", "none":
		return nil
	case "openai":
		if cfg.APIKey == "" {
			panic("moderation.driver=openai requires moderation.api_key")
		}
		return middleware.NewOpenAIModerator(cfg.APIKey, cfg.BaseURL)
	default:
		panic("unknown moderation driver: " + cfg.Driver)
	}
}

// buildOutbox constructs an OutboxPublisher based on cfg.Driver.
//
// Registers close with srv:
//   - file: closes the file handle
//   - kafka: the producer's close is auto-registered by srv.NewKafkaProducer;
//     KafkaOutbox shares that same producer reference, so it doesn't register
//     an extra AddCloser (avoiding a double close).
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
		// dual-write: file is the source of truth (sync commit), Kafka is a best-effort async broadcast.
		// The commit still succeeds if the broker is down; an external replay tool reads the file to resend (see docs/05 §5).
		fileSink, err := usage.NewFileOutbox(cfg.File.Path)
		if err != nil {
			return nil, fmt.Errorf("file_and_kafka: file sink: %w", err)
		}
		producer, err := srv.NewKafkaProducer(cfg.Kafka.KafkaConfig)
		if err != nil {
			return nil, fmt.Errorf("file_and_kafka: kafka producer: %w", err)
		}
		// The kafka leg always goes async: in dual-write mode Kafka is best-effort and must not block the file commit
		kafkaSink := usage.NewAsyncKafkaOutbox(producer, cfg.Kafka.Topic, usage.AsyncOptions{
			BufferSize:  cfg.Kafka.BufferSize,
			MaxRetries:  cfg.Kafka.MaxRetries,
			BackoffBase: cfg.Kafka.BackoffBase,
			DLQTopic:    cfg.Kafka.DLQTopic, // optional; file is already the source of truth, DLQ is just a per-message-level fallback
			Logger:      slog.Default(),
		})
		srv.AddCloser("dual-kafka-async", kafkaSink.Close)
		ob := usage.NewDualWriteOutbox(fileSink, kafkaSink, slog.Default())
		srv.AddCloser("dual-file-outbox", ob.Close) // closes only the file; kafka is managed by the line above
		return ob, nil
	default:
		return nil, fmt.Errorf("unknown usage_events driver %q (want file|kafka|async_kafka|file_and_kafka)", cfg.Driver)
	}
}
