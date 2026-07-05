// Command llm-gateway-console is the control plane (Admin API): it replaces the
// data plane's "maintain business data via direct SQL" approach with a governed
// management interface.
//
// Usage:
//
//	go run ./cmd/console -config ./configs/local/console.yaml
//
// Decoupled from the data plane (cmd/gateway) **solely via MySQL**—the control
// plane writes, the data plane reads through its TTL cache.
// Shares the KEK (data_key): the control plane uses it to encrypt endpoints.auth
// on write, and it must match the data plane's.
//
// Blank-imports a set of vendor Factory + translator packages so that
// endpointcheck.Validate's vendor-registration / translator-reachability checks
// can be decided before a write (using the same logic as the data plane).
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

	// vendor Factory registration (needed for endpointcheck's vendor_not_registered check)
	_ "github.com/zereker/llm-gateway/pkg/protocol/anthropic"
	_ "github.com/zereker/llm-gateway/pkg/protocol/azureopenai"
	_ "github.com/zereker/llm-gateway/pkg/protocol/gemini"
	_ "github.com/zereker/llm-gateway/pkg/protocol/openai"

	// translator registration (needed for endpointcheck's no_translator_path check)
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

	// Same KEK as the data plane: used to encrypt endpoints.auth on write.
	// Missing / wrong-length key fails fast.
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

	// Optional cachebus: if redis.addr is configured, attach a Publisher so
	// revocations precisely notify the data plane.
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
