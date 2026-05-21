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
// 由 admin 进程通过 cmd/admin 维护，gateway 启动期 CheckSchema + 读全量。
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

	"github.com/zereker/llm-gateway/pkg/cdc"
	"github.com/zereker/llm-gateway/pkg/config"
	"github.com/zereker/llm-gateway/pkg/contentlog"
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/health"
	"github.com/zereker/llm-gateway/pkg/middleware"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
	"github.com/zereker/llm-gateway/pkg/repo"
	"github.com/zereker/llm-gateway/pkg/router"
	"github.com/zereker/llm-gateway/pkg/selector"
	"github.com/zereker/llm-gateway/pkg/server"
	"github.com/zereker/llm-gateway/pkg/trace"
	"github.com/zereker/llm-gateway/pkg/invoker"
	"github.com/zereker/llm-gateway/pkg/usage"

	// adapter blank imports：init() 自动注册到 adapter registry
	_ "github.com/zereker/llm-gateway/pkg/protocol/anthropic"
	_ "github.com/zereker/llm-gateway/pkg/protocol/gemini"
	_ "github.com/zereker/llm-gateway/pkg/protocol/openai"

	// translator blank imports：init() 自动注册到 translator registry
	_ "github.com/zereker/llm-gateway/pkg/translator/anthropic_openai"
	_ "github.com/zereker/llm-gateway/pkg/translator/identity"
	_ "github.com/zereker/llm-gateway/pkg/translator/openai_anthropic"
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
// gateway 不拥有 schema：启动只 OpenDB + repo.CheckSchema 验证表存在；缺表
// 直接报错退出（schema 由 cmd/admin 维护）。表里没有 model_service / endpoint /
// api_key 时 gateway 仍能启动，请求过来时 M5 / M7 / M2 会 404 / 503 / 401。
//
// 任意中间步骤失败时通过 defer 把已 open 的 infra 一并 Close，避免泄漏。
func buildEngine(cfg *config.Config) (engine *gin.Engine, srv *server.Server, err error) {
	srv = server.New(slog.Default())
	defer func() {
		if err != nil {
			srv.Close()
		}
	}()

	sqldb, err := srv.OpenDB(cfg.Database)
	if err != nil {
		return nil, nil, fmt.Errorf("infra.Open: %w", err)
	}
	if err = repo.CheckSchema(context.Background(), sqldb); err != nil {
		return nil, nil, err
	}

	// M6 RateLimit 必须有 Redis；启动期 ping fail-fast
	rdb, err := srv.OpenRedis(cfg.Redis)
	if err != nil {
		return nil, nil, fmt.Errorf("infra.OpenRedis: %w", err)
	}

	apikeyProvider := repo.NewSQLAPIKeyProvider(sqldb)

	outbox, err := buildOutbox(srv, cfg.UsageEvents)
	if err != nil {
		return nil, nil, fmt.Errorf("usage outbox: %w", err)
	}

	// Content Log（docs/05 §2 + docs/08 §6）。none = 不构造，零开销
	contentLogger := buildContentLogger(srv, cfg.ContentLog)

	// Runtime Scoring（docs/03 §8）：未启用时 scorer = nil，scheduler 走纯静态 weight
	stats, scorer := buildScoring(cfg.Scoring)

	// Health Probing（docs/03 §10）：未启用时不启动 prober
	startHealthProber(srv, cfg.Health, healthListerAdapter{p: repo.NewSQLEndpointReader(sqldb)}, stats)

	// Sender 装配：Content Logger 通过 hooks 接入字节流（可选）
	senderOpts := []invoker.Option{}
	if contentLogger != nil {
		senderOpts = append(senderOpts, invoker.WithHooks(contentLogger))
	}
	sender := invoker.New(senderOpts...)

	// 三层缓存的 ModelCatalog：L1 进程内 LRU + L3 MySQL fallback。
	// L2 由 Debezium → Redis Streams 推送驱动 invalidation（不主动 SET Redis cache key）。
	rawCatalog := adaptCatalog(repo.NewSQLModelServiceReader(sqldb))
	msCache := cdc.NewTieredCache[*domain.ModelService](
		cdc.TieredConfig{Table: "model_services"},
		cdc.NewLRU[*domain.ModelService](1024),
		func(ms *domain.ModelService) string {
			if ms == nil {
				return ""
			}
			return ms.Model
		},
		func(ctx context.Context, pk string) (*domain.ModelService, error) {
			return rawCatalog.GetByModel(ctx, pk)
		},
	)
	catalog := cdcModelCatalog{cache: msCache}
	// 启动 Redis Stream consumer：监听 Debezium 推送的 model_services 表变更
	startCDCConsumer(srv, rdb, msCache)

	subs := adaptSubscriptions(repo.NewSQLSubscriptionProvider(sqldb))
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
	})

	// Dispatcher 装配（M7 业务编排：Selector + Invoker + EndpointQuota + Policy）。
	// 实现在各自的包里；cmd/gateway/dispatch_wiring.go 只做 wiring。
	dispatcher := buildDispatcher(
		adaptEndpoints(repo.NewSQLEndpointReader(sqldb)),
		sched,
		sender,
		rateStore,
		cfg.Selector.MaxAttempts,
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
		QuotaPolicies:  ratelimit.NewPolicyCache(repo.NewSQLQuotaPolicyProvider(sqldb), 0),

		// M7 Schedule (Dispatcher 编排：fallback / retry / streaming 在 pkg/dispatch 内)
		Dispatcher: dispatcher,

		// M8 Moderation
		Moderator: buildModerator(cfg.Moderation),

		// M10 Tracing
		UsageOutbox: outbox,
		AuditTracer: buildTracer(srv, cfg.Trace),
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
func buildScoring(cfg config.ScoringConfig) (selector.EndpointStatsStore, selector.Scorer) {
	if !cfg.Enabled {
		return nil, nil
	}
	decay := cfg.EMADecay
	if decay <= 0 {
		decay = 0.2
	}
	store := selector.NewInMemoryStatsStore(decay)
	baselineMs := float64(200)
	if cfg.LatencyBaseline > 0 {
		baselineMs = float64(cfg.LatencyBaseline.Milliseconds())
	}
	scorer := selector.NewDefaultScorer(store, cfg.MinSamples, baselineMs)
	return store, scorer
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

// cdcModelCatalog 把 cdc.TieredCache 适配为 middleware.ModelCatalog。
type cdcModelCatalog struct {
	cache *cdc.TieredCache[*domain.ModelService]
}

func (c cdcModelCatalog) GetByModel(ctx context.Context, model string) (*domain.ModelService, error) {
	return c.cache.Get(ctx, model)
}

// startCDCConsumer 启 Debezium → Redis Streams 消费者，把变更事件推给本进程
// 所有 TieredCache（目前只有 ModelService；后续 EndpointReader 等接入时同理添加）。
//
// Redis Stream key 命名（跟 configs/debezium/application.properties 对齐）：
//
//	llm_gateway.llm_gateway.model_services
//
// 第一个 llm_gateway 是 topic prefix；第二个是 schema 名；第三个是 table 名。
func startCDCConsumer(srv *server.Server, rdb *redis.Client, msCache *cdc.TieredCache[*domain.ModelService]) {
	consumer := cdc.NewStreamConsumer(cdc.ConsumerConfig{
		Redis: rdb,
		Streams: map[string]string{
			"llm_gateway.llm_gateway.model_services": "model_services",
			// 未来加：endpoints / account_model_subscriptions / api_keys / quota_policies / accounts
		},
		Handler: func(ctx context.Context, table string, e *cdc.Event) error {
			switch table {
			case "model_services":
				return msCache.HandleEvent(ctx, table, e)
			}
			return nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	consumer.Run(ctx)
	srv.AddCloser("cdc-consumer", func() error {
		cancel()
		consumer.Stop()
		return nil
	})
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
//               训练数据回流）交给 fluent-bit / vector，gateway 不内嵌 Kafka producer。
//               理由见 docs/architecture/05-metering-billing.md §2。
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
