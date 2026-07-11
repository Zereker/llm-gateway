package repo

import (
	"context"
	"os"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/zereker/llm-gateway/internal/infra"
)

// truncateAll disables FOREIGN_KEY_CHECKS, empties all business tables, then
// seeds the default account.
//
// A plain TRUNCATE errors out under FK relationships (even with empty child
// tables, the schema-level reference alone triggers the rejection). Bypassing
// FK checks during test setup is the conventional workaround.
//
// **default account**: many tests use testAccount="default", and other
// tables FK -> accounts(pin), so after truncating we must reseed a single
// accounts("default") row.
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
	// seed the default account (FK anchor)
	if _, err := db.Exec(`INSERT INTO accounts (pin, name) VALUES ('default', 'Default Account')`); err != nil {
		return err
	}
	return nil
}

// devDataKey is a fixed 32-byte hex KEK, for use by this package's tests only.
// Any test that reads/writes the endpoints.auth column depends on this being
// installed via init().
const devDataKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func init() {
	if err := SetDataKey(devDataKey); err != nil {
		panic("repo tests: SetDataKey: " + err.Error())
	}
}

// newTestDB spins up a *sqlx.DB connected to local MySQL, runs Migrate, and
// TRUNCATEs the business tables so each test starts from a clean state.
//
// If MYSQL_DSN isn't set, it calls t.Skip directly — all SQL tests are
// skipped when CI has no MySQL installed. For local development:
// `docker compose up -d mysql` then export MYSQL_DSN='...'.
//
// Note: all internal/repo SQL tests share the same schema; tests within this
// package run serially, so TRUNCATE at setup is sufficient isolation. Running
// across packages in parallel (`go test -p N ./...`) would have them
// truncating each other's tables — that needs -p 1 or a separate database
// per package. The Makefile's `test-integration` already runs with -p 1.
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
	// each test gets clean tables.
	// MySQL TRUNCATE rejects a table referenced by an FK (regardless of
	// whether the child table has data — the schema-level reference alone
	// triggers the rejection); disable FOREIGN_KEY_CHECKS to sweep all
	// tables, then restore it.
	if err := truncateAll(db); err != nil {
		_ = db.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
