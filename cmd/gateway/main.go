// Command ai-gateway 是数据面：接 LLM 客户端请求 → 跑 10-middleware 链 → 转发上游。
//
// 用法（最小起步）：
//
//	go run ./cmd/gateway -config ./configs/local/gateway.yaml
//
// gateway.yaml 见 configs/local/gateway.yaml；包含 server / middleware / paths / database 四段。
//
// 路由与 middleware 装配在 pkg/router；DB（model_services / endpoints）由
// admin 进程通过 cmd/admin 维护，gateway 启动期 Migrate + 读全量。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/config"
	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/infra"
	"github.com/zereker-labs/ai-gateway/pkg/middleware"
	"github.com/zereker-labs/ai-gateway/pkg/repo"
	"github.com/zereker-labs/ai-gateway/pkg/router"
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

// run 加载 config → 构造 engine → 启动 HTTP → 等待信号优雅退出。
func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	engine, cleanup, err := buildEngine(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           engine,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
	}

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("ai-gateway listening", "addr", cfg.Server.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-serverErr:
		return err
	case s := <-sig:
		slog.Info("shutdown signal", "signal", s.String())
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// buildEngine 构造所有 deps 并装配 router.NewEngine。
//
// gateway 不拥有 schema：启动只 Open + repo.CheckSchema 验证表存在；缺表
// 直接报错退出（schema 由 cmd/admin 维护）。DB 里没有 model_service /
// endpoint 时 gateway 仍能启动，请求过来时 M5 / M7 会 404 / 503。
func buildEngine(cfg *config.Config) (*gin.Engine, func(), error) {
	ctx := context.Background()

	apiKeys, err := loadAPIKeys(cfg.Paths.APIKeys)
	if err != nil {
		return nil, nil, fmt.Errorf("load apikeys: %w", err)
	}

	sqldb, err := infra.Open(infra.Driver(cfg.Database.Driver), cfg.Database.DSN)
	if err != nil {
		return nil, nil, fmt.Errorf("infra.Open: %w", err)
	}
	if err := repo.CheckSchema(ctx, sqldb); err != nil {
		_ = sqldb.Close()
		return nil, nil, err
	}

	msRepo := repo.NewSQLModelServiceRepo(sqldb)
	epRepo := repo.NewSQLEndpointRepo(sqldb)

	outbox, closeOutbox, err := buildOutbox(cfg.Outbox)
	if err != nil {
		_ = sqldb.Close()
		return nil, nil, fmt.Errorf("usage outbox: %w", err)
	}

	engine := router.NewEngine(router.Deps{
		BodyLimit: cfg.Middleware.BodyLimitBytes,
		Timeout:   cfg.Middleware.Timeout,

		Auth:         middleware.AuthDeps{Provider: repo.NewAPIKeyProvider(apiKeys)},
		Envelope:     middleware.EnvelopeDeps{Detector: middleware.DefaultDetector{}, Parser: middleware.DefaultParser{}},
		ModelService: middleware.ModelServiceDeps{Provider: msRepo},
		Schedule:     middleware.ScheduleDeps{Endpoints: epRepo},
		Tracing:      middleware.TracingDeps{Outbox: outbox, Tracer: trace.NewSlogTracer(slog.Default())},
	})

	cleanup := func() {
		_ = closeOutbox()
		_ = sqldb.Close()
	}
	return engine, cleanup, nil
}

// buildOutbox 按 cfg.Outbox.Driver 构造 OutboxPublisher + 它的 close 函数。
//
// 接口（OutboxPublisher）只声明 Publish；Close 走 io.Closer 是另一回事。
// 这里同时返回两者，省掉调用方做 io.Closer 类型断言的体操。
func buildOutbox(cfg config.OutboxConfig) (usage.OutboxPublisher, func() error, error) {
	switch cfg.Driver {
	case "file":
		ob, err := usage.NewFileOutbox(cfg.File.Path)
		if err != nil {
			return nil, nil, err
		}
		return ob, ob.Close, nil
	case "kafka":
		producer, err := infra.NewKafkaProducer(cfg.Kafka.Brokers, cfg.Kafka.Topic)
		if err != nil {
			return nil, nil, err
		}
		ob := usage.NewKafkaOutbox(producer)
		return ob, ob.Close, nil
	default:
		return nil, nil, fmt.Errorf("unknown outbox driver %q (want file|kafka)", cfg.Driver)
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
