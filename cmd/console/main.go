// Command llm-gateway-console is the control plane (Admin API): it replaces the
// data plane's "maintain business data via direct SQL" approach with a governed
// management API.
//
// Usage:
//
//	go run ./cmd/console -config ./configs/local/console.yaml
//
// Decoupled from the data plane (cmd/gateway) **only through MySQL** — the
// control plane writes, and the data plane reads through its TTL cache.
// Shared KEK (data_key): the control plane encrypts with it when writing
// endpoints.auth, and it must match the data plane's.
//
// Blank-imports a batch of vendor Factory + translator packages: this lets
// endpointcheck.Validate's vendor-registration / translator-reachability checks
// run before a write is committed (the same logic the data plane uses).
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

	// vendor Factory registration (used by endpointcheck's vendor_not_registered check)
	_ "github.com/zereker/llm-gateway/pkg/protocol/anthropic"
	_ "github.com/zereker/llm-gateway/pkg/protocol/azureopenai"
	_ "github.com/zereker/llm-gateway/pkg/protocol/bedrock"
	_ "github.com/zereker/llm-gateway/pkg/protocol/cohere"
	_ "github.com/zereker/llm-gateway/pkg/protocol/gemini"
	_ "github.com/zereker/llm-gateway/pkg/protocol/openai"

	// translator registration (used by endpointcheck's no_translator_path check)
	_ "github.com/zereker/llm-gateway/pkg/translator/anthropic_openai"
	_ "github.com/zereker/llm-gateway/pkg/translator/identity"
	_ "github.com/zereker/llm-gateway/pkg/translator/openai_anthropic"
	_ "github.com/zereker/llm-gateway/pkg/translator/openai_cohere"
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
	// Missing or wrong-length key fails fast.
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
	// key revocations notify the data plane precisely.
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
