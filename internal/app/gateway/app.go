// Package gateway assembles and runs the data-plane application.
// through the 10-middleware chain, and forwards them upstream.
//
// Usage (minimal quickstart):
//
//	go run ./cmd/gateway -config ./examples/local/configs/gateway.yaml
//
// See examples/local/configs/gateway.yaml for gateway.yaml; it has four sections —
// server / middleware / database / outbox (apikeys has moved to the DB, so
// there's no more paths.apikeys).
//
// Routing and middleware assembly live in internal/router. Gateway startup
// applies idempotent schema migrations before serving. Business data can be
// maintained through SQL or cmd/console.
//
// Lifecycle (infra Open + signal handling + reverse-order close) is handled by
// internal/app/runtime; this file owns dependency assembly only.
package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"

	appRuntime "github.com/zereker/llm-gateway/internal/app/runtime"
	"github.com/zereker/llm-gateway/internal/builtin"
	"github.com/zereker/llm-gateway/internal/cachebus"
	"github.com/zereker/llm-gateway/internal/config"
	"github.com/zereker/llm-gateway/internal/endpointcheck"
	"github.com/zereker/llm-gateway/internal/infra"
	"github.com/zereker/llm-gateway/internal/invoker"
	"github.com/zereker/llm-gateway/internal/ratelimit"
	"github.com/zereker/llm-gateway/internal/repo"
	"github.com/zereker/llm-gateway/internal/router"
	"github.com/zereker/llm-gateway/internal/routingpolicy"
	"github.com/zereker/llm-gateway/internal/selector"
)

const schemaStartupTimeout = 30 * time.Second

// Run loads configuration, assembles dependencies and serves until shutdown.
func Run(configPath string) error {
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
// returns the application Runtime so the caller can Serve or Close it
// (production) or Close (tests).
//
// gateway startup sequence: OpenDB → migrate → schema check. Gateway can still start even if the
// model_service / endpoint / api_key tables are empty — requests will then get
// 404 / 503 / 401 from M5 / M7 / M2 respectively.
//
// If any intermediate step fails, a defer Closes whatever infra has already
// been opened, to avoid leaking resources.
func buildEngine(cfg *config.Config) (engine *gin.Engine, srv *appRuntime.Runtime, err error) {
	handlers := builtin.NewLookup(cfg.Vendors.OpenAICompatible...)
	// Hold the runtime in a local variable s: on the error path
	// `return nil, nil, err` would overwrite the named return srv with nil, and
	// if a defer relied on srv it would Close nil → panic, masking the real
	// startup error. The defer only refers to s, so any early return can
	// cleanly Close whatever infra is already open.
	s := appRuntime.New(slog.Default())

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

	schemaCtx, cancelSchema := context.WithTimeout(context.Background(), schemaStartupTimeout)
	defer cancelSchema()

	if err = infra.Migrate(schemaCtx, sqldb); err != nil {
		return nil, nil, fmt.Errorf("infra.Migrate: %w", err)
	}

	if err = infra.CheckMigrationVersion(schemaCtx, sqldb); err != nil {
		return nil, nil, err
	}

	if err = repo.CheckSchema(schemaCtx, sqldb); err != nil {
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

	var routingPolicies *repo.CachedRoutingPolicyReader

	// Fast revocation: subscribe to the control plane's cachebus invalidation
	// channel and evict precisely on apikey invalidation events — this shrinks
	// the "key already revoked but data plane still has it cached" window from
	// ≤30s TTL down to sub-second. Best-effort: a subscribe failure only warns
	// (degrading to TTL-only invalidation) and doesn't block startup.
	if stop, subErr := cachebus.NewSubscriber(rdb, "", func(inv cachebus.Invalidation) {
		if inv.Kind == cachebus.KindAPIKey {
			apikeyProvider.Evict(inv.Key)
		}

		if inv.Kind == cachebus.KindRoutingPolicy && routingPolicies != nil {
			routingPolicies.EvictAll()
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
	catalog := repo.NewDomainModelReader(repo.NewCachedModelServiceReader(
		repo.NewSQLModelServiceReader(sqldb), 256, cacheTTL, cacheMetrics,
	))

	subs := adaptSubscriptions(repo.NewCachedSubscriptionProvider(
		repo.NewSQLSubscriptionProvider(sqldb), 10240, cacheTTL, cacheMetrics,
	))
	routingPolicies = repo.NewCachedRoutingPolicyReader(
		repo.NewSQLRoutingPolicyReader(sqldb), 1024, cacheTTL, cacheMetrics,
	)
	virtualModels := routingpolicy.NewResolver(routingPolicies, catalog, subs)

	endpointReader := repo.NewCachedEndpointReader(
		repo.NewSQLEndpointReader(sqldb), 1024, 4096, cacheTTL, cacheMetrics,
	)
	domainEndpoints := repo.NewDomainEndpointReader(endpointReader)

	// Startup-time endpoint config scan (docs/00 §3 step 6): protocol typos /
	// unregistered vendor / unreachable translator / metadata URL / quirks
	// compile failures — warn + metric, doesn't block startup.
	scanEndpoints(context.Background(), domainEndpoints, endpointcheck.Validator{Catalog: handlers}, slog.Default())
	// quota policy has only one caching layer: ratelimit.PolicyCache (caches the
	// **parsed** PolicyRule, TTL 30s). We feed it the SQL provider directly here
	// — we don't stack repo.CachedQuotaPolicyProvider on top, since two 30s
	// layers would mean a policy change takes up to 60s to take effect, and the
	// two layers' distinct miss semantics stacked together would be hard to debug.
	quotaPolicyReader := repo.NewSQLQuotaPolicyProvider(sqldb)
	rateStore := buildRateLimitStore(cfg.RateLimit, rdb)
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
	startHealthProber(srv, cfg.Health, repo.NewDomainEndpointReader(repo.NewSQLEndpointReader(sqldb)), stats, cooldown)

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
	// dispatch.go only does the wiring.
	// dispatchTracer shares the same trace.Tracer as the middleware AuditTracer,
	// ensuring the dispatch.request / dispatch.attempt spans and the M10 audit
	// live in the same trace tree.
	dispatchTracer := buildTracer(srv, cfg.Trace)
	dispatcher := buildDispatcher(
		domainEndpoints,
		sched,
		sender,
		rateStore,
		cfg.Selector.MaxAttempts,
		dispatchTracer,
	)

	engine = router.NewEngine(router.Deps{
		BodyLimit: cfg.Request.BodyLimitBytes,
		Timeout:   cfg.Request.Timeout,
		Handlers:  handlers,

		// M2 Auth
		IdentityProvider: apikeyProvider,

		// M4 Budget
		BudgetGate: buildBudgetGate(cfg.Budget),

		// M5 ModelService
		ModelCatalog:         catalog,
		SubscriptionChecker:  subs,
		VirtualModelResolver: virtualModels,

		// M6 Limit
		RateLimitStore: rateStore,
		QuotaPolicies:  ratelimit.NewPolicyCache(adaptQuotaPolicies(quotaPolicyReader), 0),

		// M7 Schedule (Dispatcher orchestration: fallback / retry / streaming live in internal/dispatch)
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
			{Name: config.DriverRedis, Check: func(ctx context.Context) error { return rdb.Ping(ctx).Err() }},
		},
	})

	return engine, srv, nil
}
