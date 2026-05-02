// Command ai-gateway 是数据面：接 LLM 客户端请求 → 跑 10-middleware 链 → 转发上游。
//
// 用法（最小起步）：
//
//	go run ./cmd/gateway -config ./examples/gateway.yaml
//
// gateway.yaml 见 examples/gateway.yaml；包含 server / middleware / paths 三段。
//
// 路由与 middleware 装配在 pkg/router；
// 数据文件（apikeys.json / kv/modelservice/* / kv/endpoint/*）由 paths 指定路径。
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
	"github.com/zereker-labs/ai-gateway/pkg/middleware"
	"github.com/zereker-labs/ai-gateway/pkg/repo"
	"github.com/zereker-labs/ai-gateway/pkg/router"
	"github.com/zereker-labs/ai-gateway/pkg/store"
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
func buildEngine(cfg *config.Config) (*gin.Engine, func(), error) {
	ctx := context.Background()

	apiKeys, err := loadAPIKeys(cfg.Paths.APIKeys)
	if err != nil {
		return nil, nil, fmt.Errorf("load apikeys: %w", err)
	}

	kv, err := store.NewFileKV(cfg.Paths.KVRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("new file kv: %w", err)
	}

	msp, err := repo.NewKVModelServiceProvider(ctx, kv, "modelservice")
	if err != nil {
		return nil, nil, fmt.Errorf("modelservice provider: %w", err)
	}
	epp, err := repo.NewKVEndpointProvider(ctx, kv, "endpoint")
	if err != nil {
		return nil, nil, fmt.Errorf("endpoint provider: %w", err)
	}

	outbox, err := usage.NewFileOutbox(cfg.Paths.UsageLog)
	if err != nil {
		return nil, nil, fmt.Errorf("usage outbox: %w", err)
	}

	engine := router.NewEngine(router.Deps{
		BodyLimit: cfg.Middleware.BodyLimitBytes,
		Timeout:   cfg.Middleware.Timeout,

		Auth:         middleware.AuthDeps{Provider: repo.NewAPIKeyProvider(apiKeys)},
		Envelope:     middleware.EnvelopeDeps{Detector: middleware.DefaultDetector{}, Parser: middleware.DefaultParser{}},
		ModelService: middleware.ModelServiceDeps{Provider: msp},
		Schedule:     middleware.ScheduleDeps{Endpoints: epp},
		Tracing:      middleware.TracingDeps{Outbox: outbox, Tracer: trace.NewSlogTracer(slog.Default())},
	})

	cleanup := func() { _ = outbox.Close() }
	return engine, cleanup, nil
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
