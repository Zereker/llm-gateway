// Command llm-gateway starts the data-plane process.
package main

import (
	"flag"
	"log/slog"
	"os"

	gatewayapp "github.com/zereker/llm-gateway/internal/app/gateway"
	"github.com/zereker/llm-gateway/pkg/trace"
)

func main() {
	configPath := flag.String("config", "./configs/local/gateway.yaml", "path to gateway YAML config")
	flag.Parse()

	slog.SetDefault(slog.New(trace.NewCtxHandler(slog.NewJSONHandler(os.Stderr, nil))))
	if err := gatewayapp.Run(*configPath); err != nil {
		slog.Error("llm-gateway exit", "err", err)
		os.Exit(1)
	}
}
