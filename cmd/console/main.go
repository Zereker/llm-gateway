// Command llm-gateway-console 是控制面（Admin API）：把数据面"直接 SQL 维护业务
// 数据"换成受控的管理接口。
//
// 用法：
//
//	go run ./cmd/console -config ./configs/local/console.yaml
//
// 与数据面（cmd/gateway）**只通过 MySQL 解耦**——控制面写、数据面按 TTL 缓存读。
// 共享 KEK（data_key）：控制面写 endpoints.auth 时用它加密，必须与数据面一致。
//
// blank import 一批 vendor Factory + translator：让 endpointcheck.Validate 的
// vendor 注册 / translator 可达性检查在写入前就能判定（跟数据面同一份逻辑）。
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/zereker/llm-gateway/pkg/cachebus"
	"github.com/zereker/llm-gateway/pkg/console"
	"github.com/zereker/llm-gateway/pkg/repo"
	"github.com/zereker/llm-gateway/pkg/server"
	"github.com/zereker/llm-gateway/pkg/trace"

	// vendor Factory 注册（endpointcheck vendor_not_registered 判定要用）
	_ "github.com/zereker/llm-gateway/pkg/protocol/anthropic"
	_ "github.com/zereker/llm-gateway/pkg/protocol/gemini"
	_ "github.com/zereker/llm-gateway/pkg/protocol/azureopenai"
	_ "github.com/zereker/llm-gateway/pkg/protocol/openai"

	// translator 注册（endpointcheck no_translator_path 判定要用）
	_ "github.com/zereker/llm-gateway/pkg/translator/anthropic_openai"
	_ "github.com/zereker/llm-gateway/pkg/translator/identity"
	_ "github.com/zereker/llm-gateway/pkg/translator/openai_anthropic"
	_ "github.com/zereker/llm-gateway/pkg/translator/openai_gemini"
	_ "github.com/zereker/llm-gateway/pkg/translator/responses_openai"
)

func main() {
	configPath := flag.String("config", "./configs/local/console.yaml", "path to console YAML config")
	flag.Parse()

	slog.SetDefault(slog.New(trace.NewCtxHandler(slog.NewJSONHandler(os.Stderr, nil))))

	if err := run(*configPath); err != nil {
		slog.Error("llm-gateway-console exit", "err", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := console.Load(configPath)
	if err != nil {
		return err
	}

	// 与数据面同一个 KEK：写 endpoints.auth 时加密。缺失 / 长度错 fail-fast。
	if err := repo.SetDataKey(cfg.DataKey); err != nil {
		return fmt.Errorf("load data_key: %w", err)
	}

	srv := server.New(slog.Default())
	sqldb, err := srv.OpenDB(cfg.Database)
	if err != nil {
		srv.Close()
		return fmt.Errorf("open db: %w", err)
	}

	store := console.NewStore(sqldb)

	// 可选 cachebus：配了 redis.addr 就挂 Publisher，吊销 key 时精准通知数据面。
	if cfg.Redis.Addr != "" {
		rdb, rerr := srv.OpenRedis(cfg.Redis)
		if rerr != nil {
			srv.Close()
			return fmt.Errorf("open redis: %w", rerr)
		}
		store = store.WithPublisher(cachebus.NewPublisher(rdb, ""))
		slog.Info("cachebus enabled: revocations invalidate data-plane cache")
	} else {
		slog.Info("cachebus disabled (no redis.addr); revocations rely on data-plane TTL")
	}

	engine := console.NewEngine(store, cfg.Admin.Tokens)

	slog.Info("console starting", "addr", cfg.Server.Addr)
	return srv.Serve(cfg.Server.Addr, engine, cfg.Server.ReadHeaderTimeout, cfg.Server.ShutdownTimeout)
}
