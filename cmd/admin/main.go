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

	"github.com/zereker-labs/ai-gateway/pkg/admin"
	"github.com/zereker-labs/ai-gateway/pkg/config"
	"github.com/zereker-labs/ai-gateway/pkg/infra"
	"github.com/zereker-labs/ai-gateway/pkg/repo"
	"github.com/zereker-labs/ai-gateway/pkg/server"
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

	engine, srv, err := buildEngine(cfg)
	if err != nil {
		return err
	}

	return srv.Serve(cfg.Server.Addr, engine, cfg.Server.ReadHeaderTimeout, cfg.Server.ShutdownTimeout)
}

// buildEngine 把 cfg → DB → repo → admin.NewEngine，并把 *server.Server 一并返回。
//
// admin 是 schema 的所有者：启动期 infra.Migrate 一定要跑。
// gateway 启动只 OpenDB + repo.CheckSchema 验证，不再 Migrate。
//
// 任意中间步骤失败时 defer 把已 open 的 infra 一并 Close（avoid leak）。
func buildEngine(cfg *config.AdminConfig) (engine *gin.Engine, srv *server.Server, err error) {
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
	if err = infra.Migrate(context.Background(), sqldb); err != nil {
		return nil, nil, fmt.Errorf("infra.Migrate: %w", err)
	}

	engine = admin.NewEngine(admin.Deps{
		Token:            cfg.Admin.Token,
		ModelServiceRepo: repo.NewSQLModelServiceRepo(sqldb),
		EndpointRepo:     repo.NewSQLEndpointRepo(sqldb),
	})

	return engine, srv, nil
}
