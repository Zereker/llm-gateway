package infra

import (
	"context"
	"os"
	"testing"
)

// mysqlDSN 拿环境变量；没设就 t.Skip。整个 infra 测试都走真 MySQL。
func mysqlDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("MYSQL_DSN not set; skipping MySQL integration test " +
			"(set to e.g. root:@tcp(localhost:3306)/llm_gateway?parseTime=true&charset=utf8mb4)")
	}
	return dsn
}

func TestOpen_MySQL(t *testing.T) {
	dsn := mysqlDSN(t)
	db, err := Open(DBConfig{Driver: DriverMySQL, DSN: dsn})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestOpen_UnknownDriver(t *testing.T) {
	_, err := Open(DBConfig{Driver: "nope", DSN: ""})
	if err == nil {
		t.Fatal("want error for unknown driver")
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	dsn := mysqlDSN(t)
	db, err := Open(DBConfig{Driver: DriverMySQL, DSN: dsn})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate (1st): %v", err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate (2nd): %v", err)
	}

	// MySQL 查 information_schema.tables 验证表存在
	var tables []string
	if err := db.Select(&tables,
		`SELECT table_name FROM information_schema.tables
		 WHERE table_schema = DATABASE()
		   AND table_name IN ('model_services', 'endpoints', 'api_keys', 'pricing_versions')
		 ORDER BY table_name`,
	); err != nil {
		t.Fatalf("query tables: %v", err)
	}
	want := map[string]bool{"model_services": false, "endpoints": false, "api_keys": false, "pricing_versions": false}
	for _, n := range tables {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for n, ok := range want {
		if !ok {
			t.Errorf("table %q not created (got %v)", n, tables)
		}
	}
}

func TestMigrate_TableShape(t *testing.T) {
	dsn := mysqlDSN(t)
	db, err := Open(DBConfig{Driver: DriverMySQL, DSN: dsn})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// 测试前清空，避免唯一约束冲突
	// FK 引用时 MySQL 拒 TRUNCATE 父表
	if _, err := db.Exec(`SET FOREIGN_KEY_CHECKS = 0`); err != nil {
		t.Fatalf("disable FK checks: %v", err)
	}
	for _, table := range []string{
		"pricing_versions",
		"account_model_subscriptions",
		"endpoints",
		"model_services",
		"api_keys",
		"accounts",
		"quota_policies",
	} {
		if _, err := db.Exec("TRUNCATE TABLE " + table); err != nil {
			t.Fatalf("TRUNCATE %s: %v", table, err)
		}
	}
	if _, err := db.Exec(`SET FOREIGN_KEY_CHECKS = 1`); err != nil {
		t.Fatalf("re-enable FK checks: %v", err)
	}
	// seed default account 让后续 FK insert 通过
	if _, err := db.Exec(`INSERT INTO accounts (pin, name) VALUES ('default', 'Default')`); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	// model_services：精简列（v0.3 删 account_id/group_name/spec_detail）
	_, err = db.Exec(
		`INSERT INTO model_services (service_id, model) VALUES (?, ?)`,
		"openai/gpt-4o", "gpt-4o",
	)
	if err != nil {
		t.Fatalf("insert model_services: %v", err)
	}

	// endpoints：auth 列存任意 VARCHAR 字符串（schema validation 不在 infra 层做，
	// 真实加密走 pkg/repo Scanner/Valuer）；routing 必须是合法 JSON
	_, err = db.Exec(
		`INSERT INTO endpoints (name, vendor, model, group_name, weight, enabled, auth, routing)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"openai_main", "openai", "gpt-4o", "default", 100, true,
		"v1:dummy-ciphertext", `{"url":"https://api.openai.com"}`,
	)
	if err != nil {
		t.Fatalf("insert endpoints: %v", err)
	}

	// api_keys：hash + prefix 形态
	_, err = db.Exec(
		`INSERT INTO api_keys
		 (account_id, api_key_hash, api_key_prefix, api_key_id, sub_account_id, group_name, external_user, enabled)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"default", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"sk-abcdef123", "ak_alice_test", "alice", "default", false, true,
	)
	if err != nil {
		t.Fatalf("insert api_keys: %v", err)
	}

	var msCount, epCount, akCount int
	_ = db.Get(&msCount, `SELECT COUNT(*) FROM model_services`)
	_ = db.Get(&epCount, `SELECT COUNT(*) FROM endpoints`)
	_ = db.Get(&akCount, `SELECT COUNT(*) FROM api_keys`)
	if msCount != 1 || epCount != 1 || akCount != 1 {
		t.Errorf("counts: ms=%d ep=%d ak=%d, want 1/1/1", msCount, epCount, akCount)
	}
}
