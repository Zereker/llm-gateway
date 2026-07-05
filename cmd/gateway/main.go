// Command llm-gateway 是数据面：接 LLM 客户端请求 → 跑 10-middleware 链 → 转发上游。
//
// 用法（最小起步）：
//
//	go run ./cmd/gateway -config ./configs/local/gateway.yaml
//
// gateway.yaml 见 configs/local/gateway.yaml；包含 server / middleware /
// database / outbox 四段（apikeys 已迁 DB，不再有 paths.apikeys）。
//
// 路由与 middleware 装配在 pkg/router；DB（model_services / endpoints / api_keys）
// 启动时 gateway 自己跑 infra.Migrate 建表；业务数据（model_services / endpoints /
// api_keys / pricing 等）由 deployer 直接 SQL 插入维护（本仓库不带控制平面）。
//
// lifecycle（infra Open + 信号处理 + 倒序 close）走 pkg/server，本文件只做
// 配置加载 + 业务装配 + 把 engine 交给 server。
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

	// vendor Factory blank imports：init() 自动注册到 protocol vendor registry
	_ "github.com/zereker/llm-gateway/pkg/protocol/anthropic"
	_ "github.com/zereker/llm-gateway/pkg/protocol/azureopenai"
	_ "github.com/zereker/llm-gateway/pkg/protocol/bedrock"
	_ "github.com/zereker/llm-gateway/pkg/protocol/cohere"
	_ "github.com/zereker/llm-gateway/pkg/protocol/gemini"
	_ "github.com/zereker/llm-gateway/pkg/protocol/openai"

	// translator blank imports：init() 自动注册到 translator registry
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

	// slog default：用 trace.CtxHandler 包 JSON handler，让所有 *Context 系列调用
	// （slog.InfoContext / ErrorContext 等）自动从 ctx 抽 trace_id / span_id /
	// baggage（sub_account_id / request_id 等）加进 record。
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

	// 装载 endpoints.auth 列加密 KEK；缺失或长度错 fail-fast。
	if err := repo.SetDataKey(cfg.DataKey); err != nil {
		return fmt.Errorf("load data_key: %w", err)
	}

	engine, srv, err := buildEngine(cfg)
	if err != nil {
		return err
	}

	return srv.Serve(cfg.Server.Addr, engine, cfg.Server.ReadHeaderTimeout, cfg.Server.ShutdownTimeout)
}

// buildEngine 构造 deps 并装配 router.NewEngine；同时返回 *server.Server，
// 供调用方决定 Serve（生产）或 Close（测试）。
//
// gateway 启动期：OpenDB → infra.Migrate（IF NOT EXISTS 幂等）→ repo.CheckSchema
// 验证表存在。schema 演进直接改 pkg/infra/schema.sql；表里没有
// model_service / endpoint / api_key 时 gateway 仍能启动，请求过来时 M5 / M7 / M2
// 会 404 / 503 / 401。
//
// 任意中间步骤失败时通过 defer 把已 open 的 infra 一并 Close，避免泄漏。
func buildEngine(cfg *config.Config) (engine *gin.Engine, srv *server.Server, err error) {
	// 用局部 s 持有真实 server：error 路径 `return nil, nil, err` 会把 named
	// return srv 覆盖成 nil，若 defer 依赖 srv 就会 Close nil → panic，反而盖掉
	// 真正的启动错误。defer 只认 s，任何早退都能干净 Close 已 open 的 infra。
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

	// M6 RateLimit 必须有 Redis；启动期 ping fail-fast
	rdb, err := srv.OpenRedis(cfg.Redis)
	if err != nil {
		return nil, nil, fmt.Errorf("infra.OpenRedis: %w", err)
	}

	// repo TTL LRU 缓存的 Prom counter（hit / miss / error per table）。
	// 5 个 cached wrapper 共享同一个 metrics 实例；nil 时不上报。
	cacheMetrics := newRepoCacheMetrics()

	apikeyProvider := repo.NewCachedAPIKeyProvider(
		repo.NewSQLAPIKeyProvider(sqldb), 10240, 30*time.Second, cacheMetrics,
	)

	// 快速吊销：订阅控制面的 cachebus 失效频道，收到 apikey 失效就精准 evict——
	// 把"key 已吊销但数据面仍缓存有效"的窗口从 ≤30s TTL 收到亚秒级。best-effort：
	// 订阅失败只 warn（退化成纯 TTL），不阻塞启动。
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

	// Content Log（docs/05 §2 + docs/08 §6）。none = 不构造，零开销
	contentLogger := buildContentLogger(srv, cfg.ContentLog)

	// Runtime Scoring（docs/03 §8）：未启用时 scorer = nil，scheduler 走纯静态 weight
	stats, scorer := buildScoring(cfg.Scoring, rdb)

	// Health Probing（docs/03 §10）：未启用时不启动 prober
	startHealthProber(srv, cfg.Health, healthListerAdapter{p: repo.NewSQLEndpointReader(sqldb)}, stats)

	// Sender 装配：Content Logger 通过 hooks 接入字节流（可选）
	senderOpts := []invoker.Option{}
	if contentLogger != nil {
		senderOpts = append(senderOpts, invoker.WithHooks(contentLogger))
	}
	sender := invoker.New(senderOpts...)

	// 进程内 TTL LRU 缓存——repo 唯一的缓存策略。
	// deployer SQL 改完业务表后 ≤ TTL（默认 30s）gateway 看到新值。
	// 每个 cached wrapper 自带 metrics → llm_gateway_repo_cache_total{table,result}。
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

	// 启动期 endpoint 配置扫描（docs/00 §3 step 6）：protocol typo / vendor 未注册 /
	// translator 不可达 / metadata URL / quirks 编译失败——warn + metric，不阻塞启动。
	scanEndpoints(context.Background(), endpointReader, slog.Default())
	// quota policy 只有一层缓存：ratelimit.PolicyCache（缓存**解析后**的 PolicyRule，
	// TTL 30s）。这里直接喂 SQL provider——不再叠 repo.CachedQuotaPolicyProvider，
	// 双层 30s 会让改 policy 最坏 60s 才生效，且两层各自的 miss 语义叠加难排障。
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

	// Dispatcher 装配（M7 业务编排：Selector + Invoker + EndpointQuota + Policy）。
	// 实现在各自的包里；cmd/gateway/dispatch_wiring.go 只做 wiring。
	// dispatchTracer 跟 middleware AuditTracer 共用同一个 trace.Tracer，保证
	// dispatch.request / dispatch.attempt span 跟 M10 audit 在同一棵 trace 树下。
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

		// M7 Schedule (Dispatcher 编排：fallback / retry / streaming 在 pkg/dispatch 内)
		Dispatcher: dispatcher,

		// 响应缓存中间件（M6 之后、M7 之前）：精确 / 语义 / no-op
		Cache:          buildCacheMiddleware(cfg.Cache, rdb),
		EmbeddingCache: buildEmbeddingCache(cfg.Cache, rdb),

		// M8 Moderation
		Moderator: buildModerator(cfg.Moderation),

		// M10 Tracing
		UsageOutbox: outbox,
		AuditTracer: dispatchTracer,

		// /readyz 依赖检查（docs/06 §13：readiness 检查 SQL + Redis 可达；
		// 不检查 Kafka——usage 发布失败不应摘流量）
		Readiness: []router.ReadinessChecker{
			{Name: "mysql", Check: sqldb.PingContext},
			{Name: "redis", Check: func(ctx context.Context) error { return rdb.Ping(ctx).Err() }},
		},
	})

	return engine, srv, nil
}

// buildSchedulerFilters 按 cfg.Selector.Filters 列表名字 → Filter 实例。
//
// 不在 schedule pkg 里 hardcode 这个映射——cmd 才知道有哪些 deps（redis client / store /
// cooldown manager）。新加 filter 类型时在这里加一个 case。
//
// 找不到的名字直接 panic（fail-fast；启动期暴露配置错）。
func buildSchedulerFilters(names []string, store ratelimit.Store, cd selector.CooldownManager) []selector.Filter {
	out := make([]selector.Filter, 0, len(names))
	for _, n := range names {
		switch n {
		case "cooldown":
			out = append(out, selector.NewCooldownFilter(cd))
		case "limit_read":
			out = append(out, selector.NewLimitReadFilter(store))
		case "weighted_random":
			// weighted_random 是 Selector 而非 Filter；它在 cfg.Selector 单独配，
			// 这里忽略（仅为了向后兼容旧 yaml 列表）。
			continue
		case "prefix_cache":
			out = append(out, selector.NewPrefixCacheFilter(0)) // 0 = 默认 vnodes=64
		case "busy":
			out = append(out, selector.NewBusyFilter(0)) // 0 = 默认 threshold=0.85
		default:
			panic("unknown scheduler filter: " + n)
		}
	}
	return out
}

// buildTracer 按 cfg.Driver 构造 trace.Tracer。
//
//   - slog: 默认；本地结构化日志（log/slog）
//   - otel: OpenTelemetry OTLP gRPC export，注册 srv.AddCloser 让退出时 flush
//
// 找不到的 driver 直接 panic（fail-fast）。
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

// buildScoring 构造 Runtime Scoring 的 stats store + scorer（docs/03 §8）。
//
// 未启用时返回 (nil, nil) —— scheduler 将走纯静态 weight，无任何运行时打分。
//
// **driver**（cfg.Driver，按 P5 fail-fast）：
//   - inmemory（默认）：进程内 EMA，每副本独立累积；单副本 / 容忍副本间差异用。
//   - redis：多副本共享同一份 per-endpoint EMA，打分一致；生产多副本用。
//   - 未知 driver → panic（暴露配置错）。
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

// buildCacheMiddleware 装配响应缓存中间件（M6 之后、M7 之前）。
//
//   - semantic.enabled → 语义缓存（embed prompt + cosine 命中，取代精确缓存）
//   - enabled          → 精确缓存（SHA256 body 命中）
//   - 都关              → no-op（不改链）
//
// 都 Redis-backed（多副本共享）。语义缓存的 embedder 未配好则启动 fail-fast。
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

// buildEmbeddingCache 装配 embedding 模态专用缓存——**只精确**。
//
// embeddings 天然确定（无采样），精确缓存纯收益（RAG 反复 embed 同批文本命中率高）；
// 语义缓存对 embedding 无意义（必须精确匹配）。所以不复用 buildCacheMiddleware（可能
// 是语义）——任一缓存开关打开就给 embeddings 上精确缓存，共用同一 Redis store（key
// 含 protocol|model|body，与 chat key 天然不撞）。
func buildEmbeddingCache(cfg config.CacheConfig, rdb *redis.Client) gin.HandlerFunc {
	if cfg.Enabled || cfg.Semantic.Enabled {
		return middleware.ResponseCache(respcache.NewRedisStore(rdb, "llm-gateway:respcache"), cfg.TTL)
	}
	return func(c *gin.Context) { c.Next() } // no-op
}

// buildEmbedder 装配文本向量化后端（P5 fail-fast：未知 driver panic）。
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

// buildAffinity 构造会话亲和 store（Redis-backed，多副本共享）。
//
// 未启用返回 nil —— scheduler 不粘会话。启用时客户端带 X-Gateway-Session 头即把
// 该 session 粘到同一 endpoint（软亲和：pinned endpoint 被 cooldown/排除时自动重选）。
func buildAffinity(cfg config.SessionAffinityConfig, rdb *redis.Client) selector.AffinityStore {
	if !cfg.Enabled {
		return nil
	}
	return selector.NewRedisAffinityStore(rdb, "llm-gateway:sched", cfg.TTL)
}

// healthListerAdapter 把 repo.EndpointReader → health.EndpointLister（List 返 domain.Endpoint）。
type healthListerAdapter struct{ p repo.EndpointReader }

func (a healthListerAdapter) List(ctx context.Context) ([]*domain.Endpoint, error) {
	rows, err := a.p.List(ctx)
	if err != nil {
		return nil, err
	}
	return repo.ToDomainEndpoints(rows), nil
}

// startHealthProber 按 cfg 启动 Health Prober（docs/03 §10）。
//
// 未启用时不做任何事。stats == nil 时也跳过（probe 结果没人消费没意义）。
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

// buildContentLogger 按 cfg.Driver 构造 ContentLogger（返回 nil = 不开启）。
//
//   - none/"":  返回 nil，零开销（不挂 hooks）
//   - file:     JSONL append 到本地文件；下游分流（S3 / Loki / Kafka 内容安全 /
//     训练数据回流）交给 fluent-bit / vector，gateway 不内嵌 Kafka producer。
//     理由见 docs/architecture/05-metering-billing.md §2。
//
// 找不到的 driver 直接 panic（启动期暴露配置错）。
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

// buildBudgetGate 按 cfg.Driver 构造 BudgetGate。
//
//   - alwayspass: 永远放行（默认；开发 / 无付费体系）
//   - inmemory:   进程内余额跟踪（demo / 单主账号；丢内存重启清零）
//
// 找不到的 driver 直接 panic（启动期暴露配置错）。
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

// buildModerator 按 cfg.Driver 构造 Moderator。返回 nil 时 M8 静默 pass-through。
//
//   - none:   nil（默认；不审核）
//   - openai: OpenAI moderation API client（需要 cfg.APIKey）
//
// buildModerator 组装 guardrail 链（Chain 自身是 Moderator，插进 M8 零改动）。
//
// driver 的 moderator（openai）+ 可选 denylist guard 组合：
//   - 0 个 guard → nil（M8 pass-through）
//   - 1 个 guard → 直接返回它（省 Chain 开销）
//   - ≥2 个 guard → Chain 顺序跑
func buildModerator(cfg config.ModerationConfig) middleware.Moderator {
	var guards []moderation.NamedGuard

	switch cfg.Driver {
	case "", "none":
		// 无 driver-side moderator
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
			panic("moderation.denylist: " + err.Error()) // 启动 fail-fast：坏正则
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

// buildOutbox 按 cfg.Driver 构造 OutboxPublisher。
//
// 把 close 注册进 srv：
//   - file: file 句柄关闭
//   - kafka: producer 关闭由 srv.NewKafkaProducer 自动注册；KafkaOutbox 自身共享
//     producer 引用，不再额外 AddCloser（避免双关）。
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
		// dual-write：file 是 source of truth（sync commit），Kafka 是 best-effort 异步广播。
		// broker 挂了仍能 commit；外部 replay 工具读 file 补发（详见 docs/05 §5）。
		fileSink, err := usage.NewFileOutbox(cfg.File.Path)
		if err != nil {
			return nil, fmt.Errorf("file_and_kafka: file sink: %w", err)
		}
		producer, err := srv.NewKafkaProducer(cfg.Kafka.KafkaConfig)
		if err != nil {
			return nil, fmt.Errorf("file_and_kafka: kafka producer: %w", err)
		}
		// kafka 段一律走 async：dual-write 模式下 Kafka 是 best-effort，不应阻塞 file commit
		kafkaSink := usage.NewAsyncKafkaOutbox(producer, cfg.Kafka.Topic, usage.AsyncOptions{
			BufferSize:  cfg.Kafka.BufferSize,
			MaxRetries:  cfg.Kafka.MaxRetries,
			BackoffBase: cfg.Kafka.BackoffBase,
			DLQTopic:    cfg.Kafka.DLQTopic, // 可选；file 已是 truth，DLQ 仅做单消息级兜底
			Logger:      slog.Default(),
		})
		srv.AddCloser("dual-kafka-async", kafkaSink.Close)
		ob := usage.NewDualWriteOutbox(fileSink, kafkaSink, slog.Default())
		srv.AddCloser("dual-file-outbox", ob.Close) // 只关 file；kafka 由上面那行管
		return ob, nil
	default:
		return nil, fmt.Errorf("unknown usage_events driver %q (want file|kafka|async_kafka|file_and_kafka)", cfg.Driver)
	}
}
