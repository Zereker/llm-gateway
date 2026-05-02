// Command ai-gateway 是数据面：接 LLM 客户端请求 → 跑 10-middleware 链 → 转发上游。
//
// 用法（最小起步）：
//
//	go run ./cmd/gateway -config ./configs/local/gateway.yaml
//
// gateway.yaml 见 configs/local/gateway.yaml；包含 server / middleware / paths /
// database / outbox 五段。
//
// 路由与 middleware 装配在 pkg/router；DB（model_services / endpoints）由
// admin 进程通过 cmd/admin 维护，gateway 启动期 CheckSchema + 读全量。
//
// lifecycle（infra Open + 信号处理 + 倒序 close）走 pkg/server，本文件只做
// 配置加载 + 业务装配 + 把 engine 交给 server。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/config"
	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/middleware"
	"github.com/zereker-labs/ai-gateway/pkg/repo"
	"github.com/zereker-labs/ai-gateway/pkg/router"
	"github.com/zereker-labs/ai-gateway/pkg/server"
	"github.com/zereker-labs/ai-gateway/pkg/trace"
	"github.com/zereker-labs/ai-gateway/pkg/usage"

	// adapter blank imports：init() 自动注册到 adapter registry
	_ "github.com/zereker-labs/ai-gateway/pkg/adapter/openai"
)

func main() {
	configPath := flag.String("config", "./configs/local/gateway.yaml", "path to gateway YAML config")
	flag.Parse()

	if err := run(*configPath); err != nil {
		slog.Error("ai-gateway exit", "err", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
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
// 直接报错退出（schema 由 cmd/admin 维护）。DB 里没有 model_service /
// endpoint 时 gateway 仍能启动，请求过来时 M5 / M7 会 404 / 503。
//
// 任意中间步骤失败时通过 defer 把已 open 的 infra 一并 Close，避免泄漏。
func buildEngine(cfg *config.Config) (engine *gin.Engine, srv *server.Server, err error) {
	srv = server.New(slog.Default())
	defer func() {
		if err != nil {
			srv.Close()
		}
	}()

	apiKeys, err := loadAPIKeys(cfg.Paths.APIKeys)
	if err != nil {
		return nil, nil, fmt.Errorf("load apikeys: %w", err)
	}

	sqldb, err := srv.OpenDB(cfg.Database)
	if err != nil {
		return nil, nil, fmt.Errorf("infra.Open: %w", err)
	}
	if err = repo.CheckSchema(context.Background(), sqldb); err != nil {
		return nil, nil, err
	}

	outbox, err := buildOutbox(srv, cfg.Outbox)
	if err != nil {
		return nil, nil, fmt.Errorf("usage outbox: %w", err)
	}

	engine = router.NewEngine(router.Deps{
		BodyLimit: cfg.Middleware.BodyLimitBytes,
		Timeout:   cfg.Middleware.Timeout,

		Auth:         middleware.AuthDeps{Provider: repo.NewAPIKeyProvider(apiKeys)},
		Envelope:     middleware.EnvelopeDeps{Detector: middleware.DefaultDetector{}, Parser: middleware.DefaultParser{}},
		ModelService: middleware.ModelServiceDeps{Provider: repo.NewSQLModelServiceReader(sqldb)},
		Schedule:     middleware.ScheduleDeps{Endpoints: repo.NewSQLEndpointReader(sqldb)},
		Tracing:      middleware.TracingDeps{Outbox: outbox, Tracer: trace.NewSlogTracer(slog.Default())},
	})

	return engine, srv, nil
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
		return usage.NewKafkaOutbox(producer, cfg.Kafka.Topic), nil
	default:
		return nil, fmt.Errorf("unknown outbox driver %q (want file|kafka)", cfg.Driver)
	}
}

func loadAPIKeys(path string) (map[string]domain.UserIdentity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var keys map[string]domain.UserIdentity
	if err := json.Unmarshal(data, &keys); err != nil {
		return nil, err
	}
	return keys, nil
}
