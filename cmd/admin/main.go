// Command llm-gateway-admin 是控制平面：通过 HTTP CRUD 维护
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
// 数据层：
//   - infra.Open 拿 *sqlx.DB（用于 Migrate 跑 raw SQL）
//   - gorm.Open 复用同一份 *sql.DB（用于 admin 的 CRUD store）
//   - 两套库共用一份连接池
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/gin-gonic/gin"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"

	"github.com/zereker/llm-gateway/pkg/admin"
	"github.com/zereker/llm-gateway/pkg/config"
	"github.com/zereker/llm-gateway/pkg/infra"
	"github.com/zereker/llm-gateway/pkg/repo"
	"github.com/zereker/llm-gateway/pkg/server"
)

func main() {
	configPath := flag.String("config", "./configs/local/admin.yaml", "path to admin YAML config")
	flag.Parse()

	if err := run(*configPath); err != nil {
		slog.Error("llm-gateway-admin exit", "err", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.LoadAdmin(configPath)
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

// buildEngine 把 cfg → DB → gorm Stores → admin.NewEngine。
//
// admin 是 schema 的所有者：启动期 infra.Migrate 一定要跑（用 *sqlx.DB）。
// CRUD 走 gorm，gorm 复用 *sqlx.DB.DB（同一份 *sql.DB 连接池）。
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

	gdb, err := gorm.Open(gormmysql.New(gormmysql.Config{Conn: sqldb.DB}), &gorm.Config{})
	if err != nil {
		return nil, nil, fmt.Errorf("gorm.Open: %w", err)
	}

	engine = admin.NewEngine(admin.Deps{
		Token:             cfg.Admin.Token,
		TenantStore:       admin.NewTenantStore(gdb),
		QuotaPolicyStore:  admin.NewQuotaPolicyStore(gdb),
		ModelServiceStore: admin.NewModelServiceStore(gdb),
		SubscriptionStore: admin.NewSubscriptionStore(gdb),
		EndpointStore:     admin.NewEndpointStore(gdb),
		APIKeyStore:       admin.NewAPIKeyStore(gdb),
		PricingStore:      admin.NewPricingStore(gdb),
	})

	return engine, srv, nil
}
