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

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/config"
	"github.com/zereker/llm-gateway/pkg/middleware"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
	"github.com/zereker/llm-gateway/pkg/repo"
	"github.com/zereker/llm-gateway/pkg/router"
	"github.com/zereker/llm-gateway/pkg/schedule"
	"github.com/zereker/llm-gateway/pkg/server"
	"github.com/zereker/llm-gateway/pkg/trace"
	"github.com/zereker/llm-gateway/pkg/upstream"
	"github.com/zereker/llm-gateway/pkg/usage"

	// adapter blank imports：init() 自动注册到 adapter registry
	_ "github.com/zereker/llm-gateway/pkg/adapter/anthropic"
	_ "github.com/zereker/llm-gateway/pkg/adapter/gemini"
	_ "github.com/zereker/llm-gateway/pkg/adapter/openai"

	// translator blank imports：init() 自动注册到 translator registry
	_ "github.com/zereker/llm-gateway/pkg/translator/anthropic_openai"
	_ "github.com/zereker/llm-gateway/pkg/translator/identity"
	_ "github.com/zereker/llm-gateway/pkg/translator/openai_anthropic"
	_ "github.com/zereker/llm-gateway/pkg/translator/openai_gemini"
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

	outbox, err := buildOutbox(srv, cfg.Outbox)
	if err != nil {
		return nil, nil, fmt.Errorf("usage outbox: %w", err)
	}

	engine = router.NewEngine(router.Deps{
		BodyLimit: cfg.Middleware.BodyLimitBytes,
		Timeout:   cfg.Middleware.Timeout,

		Auth: middleware.AuthDeps{Provider: apikeyProvider},
		// M4 Budget：driver 决定实现。alwayspass = 永远放行；inmemory = 进程内余额跟踪
		Budget: middleware.BudgetDeps{Gate: buildBudgetGate(cfg.Budget)},
		// M8 Moderation：driver 决定实现。none = pass-through；openai = 调 OpenAI moderation API
		Moderation: middleware.ModerationDeps{Moderator: buildModerator(cfg.Moderation)},
		// M5：catalog + subscription（pricing 不在请求路径，docs/01 §7 + docs/05 §6）
		ModelService: middleware.ModelServiceDeps{
			Catalog:       middleware.AdaptRepoCatalog(repo.NewSQLModelServiceReader(sqldb)),
			Subscriptions: middleware.AdaptRepoSubscriptions(repo.NewSQLSubscriptionProvider(sqldb)),
		},
		// M6 RateLimit：Redis 唯一实现 + PolicyCache 包一层 LRU+TTL（30s 默认）
		Limit: middleware.LimitDeps{
			Store:    ratelimit.NewRedisStore(rdb),
			Policies: ratelimit.NewPolicyCache(repo.NewSQLQuotaPolicyProvider(sqldb), 0),
		},
		Schedule: middleware.ScheduleDeps{
			Endpoints: repo.NewSQLEndpointReader(sqldb),
			Scheduler: schedule.New(schedule.Config{
				Filters: buildSchedulerFilters(
					cfg.Scheduler.Filters,
					ratelimit.NewRedisStore(rdb),
					schedule.NewRedisCooldownManager(rdb, schedule.CooldownDurations{
						Transient: cfg.Scheduler.Cooldown.Transient,
						Capacity:  cfg.Scheduler.Cooldown.Capacity,
						Permanent: cfg.Scheduler.Cooldown.Permanent,
						Invalid:   cfg.Scheduler.Cooldown.Invalid,
						Unknown:   cfg.Scheduler.Cooldown.Unknown,
					}),
				),
				Cooldown: schedule.NewRedisCooldownManager(rdb, schedule.CooldownDurations{
					Transient: cfg.Scheduler.Cooldown.Transient,
					Capacity:  cfg.Scheduler.Cooldown.Capacity,
					Permanent: cfg.Scheduler.Cooldown.Permanent,
					Invalid:   cfg.Scheduler.Cooldown.Invalid,
					Unknown:   cfg.Scheduler.Cooldown.Unknown,
				}),
				MaxAttempts:    cfg.Scheduler.MaxAttempts,
				MaxPerEndpoint: cfg.Scheduler.MaxPerEndpoint,
			}),
			// Sender 默认走 adapter 全局 registry + http.DefaultClient；
			// 后续要换 client（自签名 CA / proxy / mTLS）在此 New(WithHTTPClient(...))
			Sender: upstream.New(),
		},
		Tracing: middleware.TracingDeps{
			Outbox: outbox,
			Tracer: buildTracer(srv, cfg.Trace),
		},
	})

	return engine, srv, nil
}

// buildSchedulerFilters 按 cfg.Scheduler.Filters 列表名字 → Filter 实例。
//
// 不在 schedule pkg 里 hardcode 这个映射——cmd 才知道有哪些 deps（redis client / store /
// cooldown manager）。新加 filter 类型时在这里加一个 case。
//
// 找不到的名字直接 panic（fail-fast；启动期暴露配置错）。
func buildSchedulerFilters(names []string, store ratelimit.Store, cd schedule.CooldownManager) []schedule.Filter {
	out := make([]schedule.Filter, 0, len(names))
	for _, n := range names {
		switch n {
		case "cooldown":
			out = append(out, schedule.NewCooldownFilter(cd))
		case "limit_read":
			out = append(out, schedule.NewLimitReadFilter(store))
		case "weighted_random":
			out = append(out, schedule.NewWeightedRandomSelector())
		case "prefix_cache":
			out = append(out, schedule.NewPrefixCacheFilter(0)) // 0 = 默认 vnodes=64
		case "busy":
			out = append(out, schedule.NewBusyFilter(0)) // 0 = 默认 threshold=0.85
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
func buildOutbox(srv *server.Server, cfg config.OutboxConfig) (usage.OutboxPublisher, error) {
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
	default:
		return nil, fmt.Errorf("unknown outbox driver %q (want file|kafka)", cfg.Driver)
	}
}
