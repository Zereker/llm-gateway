// Command ai-gateway 是数据面：接 LLM 客户端请求 → 跑 10-middleware 链 → 转发上游。
//
// 用法（最小起步）：
//
//	go run ./cmd/gateway -config ./examples
//
// 配置目录结构：
//
//	<config>/apikeys.json                 // map[apiKey]UserIdentity
//	<config>/kv/modelservice/<id>.json    // domain.ModelServiceSnapshot
//	<config>/kv/endpoint/<id>.json        // domain.Endpoint
//
// 路由：
//
//	GET  /healthz   liveness
//	GET  /readyz    readiness
//	GET  /metrics   Prometheus（v0.1 占位）
//	*    /v1/*      LLM API（走完整 middleware 链）
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
	"path/filepath"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/middleware"
	"github.com/zereker-labs/ai-gateway/pkg/store"
	"github.com/zereker-labs/ai-gateway/pkg/trace"
	"github.com/zereker-labs/ai-gateway/pkg/usage"

	// adapter blank imports：init() 自动注册到 adapter registry
	_ "github.com/zereker-labs/ai-gateway/pkg/adapter/openai"
)

type options struct {
	Addr      string
	ConfigDir string
	UsageLog  string
	BodyLimit int64
	Timeout   time.Duration
}

func main() {
	opts := parseFlags()
	if err := run(opts); err != nil {
		slog.Error("ai-gateway exit", "err", err)
		os.Exit(1)
	}
}

func parseFlags() options {
	opts := options{}
	flag.StringVar(&opts.Addr, "addr", ":8080", "HTTP listen address")
	flag.StringVar(&opts.ConfigDir, "config", "./examples", "config root (apikeys.json + kv/)")
	flag.StringVar(&opts.UsageLog, "usage-log", "/tmp/ai-gateway-usage.log", "usage events JSONL output")
	flag.Int64Var(&opts.BodyLimit, "body-limit", 10<<20, "max request body bytes")
	flag.DurationVar(&opts.Timeout, "timeout", 60*time.Second, "gateway-level request timeout")
	flag.Parse()
	return opts
}

// run 启动 HTTP 服务并等待 SIGINT / SIGTERM 优雅退出。
func run(opts options) error {
	engine, cleanup, err := buildEngine(opts)
	if err != nil {
		return err
	}
	defer cleanup()

	srv := &http.Server{Addr: opts.Addr, Handler: engine, ReadHeaderTimeout: 10 * time.Second}

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("ai-gateway listening", "addr", opts.Addr)
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

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// buildEngine 构造 gin.Engine + 装配所有 middleware。返回 cleanup 关闭 outbox 等资源。
func buildEngine(opts options) (*gin.Engine, func(), error) {
	ctx := context.Background()

	apiKeys, err := loadAPIKeys(filepath.Join(opts.ConfigDir, "apikeys.json"))
	if err != nil {
		return nil, nil, fmt.Errorf("load apikeys: %w", err)
	}

	kv, err := store.NewFileKV(filepath.Join(opts.ConfigDir, "kv"))
	if err != nil {
		return nil, nil, fmt.Errorf("new file kv: %w", err)
	}

	msp, err := middleware.NewKVModelServiceProvider(ctx, kv, "modelservice")
	if err != nil {
		return nil, nil, fmt.Errorf("modelservice provider: %w", err)
	}
	epp, err := middleware.NewKVEndpointProvider(ctx, kv, "endpoint")
	if err != nil {
		return nil, nil, fmt.Errorf("endpoint provider: %w", err)
	}

	outbox, err := usage.NewFileOutbox(opts.UsageLog)
	if err != nil {
		return nil, nil, fmt.Errorf("usage outbox: %w", err)
	}

	tracer := trace.NewSlogTracer(slog.Default())

	if gin.Mode() == gin.DebugMode {
		gin.SetMode(gin.ReleaseMode)
	}
	engine := gin.New()

	// 操作端点：不走主 middleware 链
	engine.GET("/healthz", func(c *gin.Context) { c.String(200, "ok") })
	engine.GET("/readyz", func(c *gin.Context) { c.String(200, "ok") })
	engine.GET("/metrics", func(c *gin.Context) {
		c.Data(200, "text/plain; version=0.0.4", []byte("# v0.1 metric endpoint stub\n"))
	})

	// LLM API 路由组：走完整 middleware 链
	api := engine.Group("/v1",
		bodyLimitMW(opts.BodyLimit),
		timeoutMW(opts.Timeout),
		middleware.TraceContext(),
		middleware.Recover(),
		middleware.Auth(middleware.AuthDeps{Provider: middleware.NewAPIKeyProvider(apiKeys)}),
		middleware.Envelope(middleware.EnvelopeDeps{Detector: middleware.DefaultDetector{}, Parser: middleware.DefaultParser{}}),
		// M4 Budget: AlwaysPassGate（NoOp，省略注册）
		middleware.ModelService(middleware.ModelServiceDeps{Provider: msp}),
		// M6 Limit: 暂未注册（v0.5+ 加 Checker）
		// M8 Moderation: 暂未注册（可选）
		middleware.Schedule(middleware.ScheduleDeps{Endpoints: epp}),
		middleware.Tracing(middleware.TracingDeps{Outbox: outbox, Tracer: tracer}),
	)
	api.Any("/*path", noopHandler)

	cleanup := func() {
		_ = outbox.Close()
	}
	return engine, cleanup, nil
}

// noopHandler M7 Schedule 已经写完响应；这里只是给 gin 一个匹配的 handler 让 middleware 链跑完。
func noopHandler(c *gin.Context) {}

// loadAPIKeys 从 path 读 JSON：map[apiKeyString]UserIdentity。
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

// bodyLimitMW 限制请求体大小；超限读到 EOF 后返回 413（由 http.MaxBytesReader 触发）。
func bodyLimitMW(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		c.Next()
	}
}

// timeoutMW 给请求 ctx 加截止时间；上游调用与 RC.Ctx 都会感知到。
func timeoutMW(d time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), d)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
