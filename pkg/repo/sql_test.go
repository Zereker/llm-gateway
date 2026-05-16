package repo

import (
	"context"
	"os"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/zereker/llm-gateway/pkg/infra"
)

// truncateAll 关 FOREIGN_KEY_CHECKS 后清空所有业务表，再 seed default account。
//
// FK 关系下普通 TRUNCATE 会报错（即使子表为空，schema-level reference 已经触发拒绝）。
// 测试 setup 阶段绕过 FK check 是惯例做法。
//
// **default account**：很多测试用 testAccount="default"，其它表 FK → accounts(pin)，所以
// truncate 后必须重新 seed 一行 accounts("default")。
func truncateAll(db *sqlx.DB) error {
	if _, err := db.Exec(`SET FOREIGN_KEY_CHECKS = 0`); err != nil {
		return err
	}
	defer func() { _, _ = db.Exec(`SET FOREIGN_KEY_CHECKS = 1`) }()

	for _, table := range []string{
		"pricing_versions",
		"account_model_subscriptions",
		"endpoints",
		"api_keys",
		"model_services",
		"accounts",
		"quota_policies",
	} {
		if _, err := db.Exec("TRUNCATE TABLE " + table); err != nil {
			return err
		}
	}
	// seed default account（FK 锚点）
	if _, err := db.Exec(`INSERT INTO accounts (pin, name) VALUES ('default', 'Default Account')`); err != nil {
		return err
	}
	return nil
}

// devDataKey 是一个固定的 32 字节 hex KEK，仅供本包 tests 用。
// 任何走 endpoints.auth 列读写的测试都依赖这个被 init() 装上。
const devDataKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func init() {
	if err := SetDataKey(devDataKey); err != nil {
		panic("repo tests: SetDataKey: " + err.Error())
	}
}

// newTestDB 起一份连到本地 MySQL 的 *sqlx.DB，跑 Migrate 并 TRUNCATE
// 三张业务表让每个测试拿到干净状态。
//
// 没设 MYSQL_DSN 环境变量时直接 t.Skip——CI 没装 MySQL 时全部 SQL 测试跳过；
// 本地开发：`docker compose up -d mysql` 后 export MYSQL_DSN='...' 即可。
//
// 注意：所有 pkg/repo 的 SQL 测试共享同一个 schema；同包测试串行跑，
// TRUNCATE 在 setup 足以隔离；跨包并行（`go test -p N ./...`）会互相 truncate，
// 需要 -p 1 或各自独立 database。Makefile 的 `test-integration` 已按 -p 1 跑。
func newTestDB(t *testing.T) *sqlx.DB {
	t.Helper()

	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("MYSQL_DSN not set; skipping MySQL integration test " +
			"(set to e.g. root:@tcp(localhost:3306)/llm_gateway?parseTime=true&charset=utf8mb4)")
	}

	db, err := infra.Open(infra.DBConfig{Driver: infra.DriverMySQL, DSN: dsn})
	if err != nil {
		t.Fatalf("infra.Open: %v", err)
	}
	if err := infra.Migrate(context.Background(), db); err != nil {
		_ = db.Close()
		t.Fatalf("infra.Migrate: %v", err)
	}
	// 每个测试拿干净表。
	// MySQL TRUNCATE 拒绝被 FK 引用的表（不看子表有没有数据，schema-level 拒绝）；
	// 关掉 FOREIGN_KEY_CHECKS 一把扫所有表，再恢复。
	if err := truncateAll(db); err != nil {
		_ = db.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
