// Command llm-gateway starts the data-plane process.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	gatewayapp "github.com/zereker/llm-gateway/internal/app/gateway"
	"github.com/zereker/llm-gateway/internal/trace"
	"github.com/zereker/llm-gateway/internal/version"
)

func main() {
	configPath := flag.String("config", "./examples/local/configs/gateway.yaml", "path to gateway YAML config")
	showVersion := flag.Bool("version", false, "print version and build metadata")

	flag.Parse()

	if *showVersion {
		fmt.Println(version.String("llm-gateway"))

		return
	}

	slog.SetDefault(slog.New(trace.NewCtxHandler(slog.NewJSONHandler(os.Stderr, nil))))

	if err := gatewayapp.Run(*configPath); err != nil {
		slog.Error("llm-gateway exit", "err", err)
		os.Exit(1)
	}
}
