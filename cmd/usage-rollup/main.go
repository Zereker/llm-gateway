// Command llm-gateway-usage-rollup 把 usage outbox（JSONL 文件）增量聚合进
// usage_daily 表，喂控制面 dashboard。
//
// 用法：
//
//	# 跑一次退出（配合外部 cron）
//	go run ./cmd/usage-rollup -dsn '...' -file /var/log/llm-gateway/usage.jsonl
//
//	# 常驻，每 30s 增量聚合一次
//	go run ./cmd/usage-rollup -dsn '...' -file /var/log/llm-gateway/usage.jsonl -interval 30s
//
// 增量 + 幂等：sidecar offset 文件记住已处理位置，反复跑不重复计数。
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zereker/llm-gateway/pkg/infra"
	"github.com/zereker/llm-gateway/pkg/usagerollup"
)

func main() {
	dsn := flag.String("dsn", os.Getenv("LLM_GATEWAY_DATABASE_DSN"), "MySQL DSN (or LLM_GATEWAY_DATABASE_DSN)")
	file := flag.String("file", "", "usage outbox JSONL path (source of truth)")
	interval := flag.Duration("interval", 0, "poll interval; 0 = run once and exit")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	if *dsn == "" || *file == "" {
		slog.Error("usage-rollup: -dsn and -file are required")
		os.Exit(2)
	}

	if err := run(*dsn, *file, *interval); err != nil {
		slog.Error("usage-rollup exit", "err", err)
		os.Exit(1)
	}
}

func run(dsn, file string, interval time.Duration) error {
	db, err := infra.Open(infra.DBConfig{Driver: infra.DriverMySQL, DSN: dsn})
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// usage_daily 表存在性由 Migrate 幂等保证（跟 gateway / console 同一份 schema）。
	if err := infra.Migrate(ctx, db); err != nil {
		return err
	}

	rollup := usagerollup.New(db, file)

	for {
		res, err := rollup.Run(ctx)
		if err != nil {
			slog.Error("rollup run failed", "err", err)
		} else {
			slog.Info("rollup complete",
				"events", res.Events, "skipped", res.Skipped,
				"bytes", res.BytesRead, "offset", res.NewOffset)
		}

		if interval <= 0 {
			return err // run-once 模式
		}
		select {
		case <-ctx.Done():
			slog.Info("usage-rollup shutting down")
			return nil
		case <-time.After(interval):
		}
	}
}
