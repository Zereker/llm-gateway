// Command llm-gateway-migrate applies pending database schema migrations.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/zereker/llm-gateway/pkg/config"
	"github.com/zereker/llm-gateway/pkg/infra"
)

func main() {
	configPath := flag.String("config", "./configs/local/gateway.yaml", "path to gateway YAML config")
	flag.Parse()
	if err := run(*configPath); err != nil {
		slog.Error("migration failed", "err", err)
		os.Exit(1)
	}
	slog.Info("database schema is current")
}

func run(configPath string) error {
	database, err := config.LoadDatabase(configPath)
	if err != nil {
		return err
	}
	db, err := infra.Open(database)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	if err := infra.Migrate(context.Background(), db); err != nil {
		return err
	}
	return infra.CheckMigrationVersion(context.Background(), db)
}
