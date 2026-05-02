// Command ai-gateway-admin 是控制平面：通过 HTTP CRUD 维护
// model_services / endpoints 表；schema 演进也归 admin（启动期 infra.Migrate）。
//
// 用法：
//
//	go run ./cmd/admin -config ./configs/local/admin.yaml
//
// admin 跟 gateway 完全独立——独立 binary、独立 yaml、独立端口（默认 :8081）。
// 两边只共享数据库（database 段必须跟 gateway.yaml 写一致）。
//
// 部署假设：admin 服务**只暴露内网 / loopback**；X-Admin-Token 是次要保险，
// 主防线是网络隔离。
package main

import (
	"context"
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
	"github.com/zereker-labs/ai-gateway/pkg/infra"
	"github.com/zereker-labs/ai-gateway/pkg/repo"
)

func main() {
	configPath := flag.String("config", "./configs/local/admin.yaml", "path to admin YAML config")
	flag.Parse()

	if err := run(*configPath); err != nil {
		slog.Error("ai-gateway-admin exit", "err", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.LoadAdmin(configPath)
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
		slog.Info("ai-gateway-admin listening", "addr", cfg.Server.Addr)
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

// buildEngine 装配 gin engine：连 DB → Migrate → 注册路由。
//
// admin 是 schema 的所有者：启动期 infra.Migrate 一定要跑。
// gateway 启动只 Open + repo.CheckSchema 验证，不再 Migrate。
func buildEngine(cfg *config.AdminConfig) (*gin.Engine, func(), error) {
	ctx := context.Background()

	sqldb, err := infra.Open(infra.Driver(cfg.Database.Driver), cfg.Database.DSN)
	if err != nil {
		return nil, nil, fmt.Errorf("infra.Open: %w", err)
	}
	if err := infra.Migrate(ctx, sqldb); err != nil {
		_ = sqldb.Close()
		return nil, nil, fmt.Errorf("infra.Migrate: %w", err)
	}

	msRepo := repo.NewSQLModelServiceRepo(sqldb)
	epRepo := repo.NewSQLEndpointRepo(sqldb)

	if gin.Mode() == gin.DebugMode {
		gin.SetMode(gin.ReleaseMode)
	}
	engine := gin.New()
	engine.Use(gin.Recovery())

	// ops endpoints 不走 admin 鉴权
	engine.GET("/healthz", func(c *gin.Context) { c.String(200, "ok") })
	engine.GET("/readyz", func(c *gin.Context) { c.String(200, "ok") })

	api := engine.Group("/admin/v1", adminAuthMW(cfg.Admin.Token))
	registerModelServiceRoutes(api, msRepo)
	registerEndpointRoutes(api, epRepo)

	cleanup := func() { _ = sqldb.Close() }
	return engine, cleanup, nil
}
