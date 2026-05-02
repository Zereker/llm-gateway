package repo

import (
	"context"
	"os"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/zereker-labs/ai-gateway/pkg/infra"
)

// newTestDB 起一份连到本地 MySQL 的 *sqlx.DB，跑 Migrate 并 TRUNCATE
// model_services / endpoints 让每个测试拿到干净状态。
//
// 没设 MYSQL_DSN 环境变量时直接 t.Skip——CI 没装 MySQL 时全部 SQL 测试跳过；
// 本地开发：`docker compose up -d mysql` 后 export MYSQL_DSN='...' 即可。
//
// 注意：所有 pkg/repo 的 SQL 测试共享同一个 schema；同包测试串行跑（go test
// 默认 within-package 是串行），TRUNCATE 在 setup 足以隔离；跨包并行
// （`go test -p N ./...`）会互相 truncate，需要 -p 1 或各自独立 database。
// Makefile 的 `test-integration` 已按 -p 1 跑。
func newTestDB(t *testing.T) *sqlx.DB {
	t.Helper()

	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("MYSQL_DSN not set; skipping MySQL integration test " +
			"(set to e.g. root:@tcp(localhost:3306)/ai_gateway?parseTime=true&charset=utf8mb4)")
	}

	db, err := infra.Open(infra.DBConfig{Driver: infra.DriverMySQL, DSN: dsn})
	if err != nil {
		t.Fatalf("infra.Open: %v", err)
	}
	if err := infra.Migrate(context.Background(), db); err != nil {
		_ = db.Close()
		t.Fatalf("infra.Migrate: %v", err)
	}
	// 每个测试拿干净表
	for _, table := range []string{"endpoints", "model_services"} {
		if _, err := db.Exec("TRUNCATE TABLE " + table); err != nil {
			_ = db.Close()
			t.Fatalf("TRUNCATE %s: %v", table, err)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
