package infra

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

// mysqlDSN reads the environment variable; t.Skip if unset. All infra tests
// go through a real MySQL instance.
func mysqlDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("MYSQL_DSN not set; skipping MySQL integration test " +
			"(set to e.g. root:@tcp(localhost:3306)/llm_gateway_test?parseTime=true&charset=utf8mb4)")
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

func TestSchemaMigrationCatalogIsSequential(t *testing.T) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != len(schemaMigrations) {
		t.Fatalf("embedded migration files = %d, catalog entries = %d", len(entries), len(schemaMigrations))
	}

	for i, migration := range schemaMigrations {
		wantVersion := i + 1
		if migration.version != wantVersion {
			t.Fatalf("migration[%d].version = %d, want %d", i, migration.version, wantVersion)
		}
		wantPrefix := fmt.Sprintf("%06d_", wantVersion)
		if !strings.HasPrefix(entries[i].Name(), wantPrefix) || migration.file != "migrations/"+entries[i].Name() {
			t.Fatalf("migration[%d] file = %q, embedded file = %q", i, migration.file, entries[i].Name())
		}
	}

	if schemaMigrations[len(schemaMigrations)-1].version != latestSchemaVersion {
		t.Fatalf("latestSchemaVersion = %d, catalog ends at %d", latestSchemaVersion, schemaMigrations[len(schemaMigrations)-1].version)
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	db, err := Open(isolatedMySQLConfig(t, "idempotent"))
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
	if err := CheckMigrationVersion(ctx, db); err != nil {
		t.Fatalf("CheckMigrationVersion: %v", err)
	}

	// Query information_schema.tables on MySQL to verify the tables exist
	var tables []string
	if err := db.Select(&tables,
		`SELECT table_name FROM information_schema.tables
		 WHERE table_schema = DATABASE()
		   AND table_name IN ('model_services', 'endpoints', 'api_keys', 'pricing_versions', 'routing_cost_profiles')
		 ORDER BY table_name`,
	); err != nil {
		t.Fatalf("query tables: %v", err)
	}
	want := map[string]bool{"model_services": false, "endpoints": false, "api_keys": false, "pricing_versions": false, "routing_cost_profiles": false}
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

func TestMigrateReportsMigrationFileError(t *testing.T) {
	db, err := Open(isolatedMySQLConfig(t, "missing_migration"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	original := schemaMigrations
	schemaMigrations = []schemaMigration{{version: 1, file: "migrations/missing.sql"}}
	t.Cleanup(func() { schemaMigrations = original })

	err = Migrate(context.Background(), db)
	if err == nil || !strings.Contains(err.Error(), "apply schema migration 1") || !strings.Contains(err.Error(), "migrations/missing.sql") {
		t.Fatalf("Migrate() error = %v, want migration version and file context", err)
	}
}

func TestApplyMigrationReportsExecutionError(t *testing.T) {
	db, err := Open(DBConfig{Driver: DriverMySQL, DSN: mysqlDSN(t)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = applyMigration(ctx, db, schemaMigrations[0])
	if err == nil || !strings.Contains(err.Error(), "execute migrations/000001_base.sql") || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("applyMigration() error = %v, want statement and cancellation context", err)
	}
}

func TestRecordMigrationReportsDatabaseError(t *testing.T) {
	db, err := Open(DBConfig{Driver: DriverMySQL, DSN: mysqlDSN(t)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err = recordMigration(context.Background(), db, 1)
	if err == nil || !strings.Contains(err.Error(), "record schema migration 1") {
		t.Fatalf("recordMigration() error = %v, want migration version context", err)
	}
}

func TestCheckMigrationVersionRejectsPreReleaseHistory(t *testing.T) {
	tests := []struct {
		name     string
		versions []int
	}{
		{name: "extra old versions", versions: []int{1, 5}},
		{name: "wrong baseline version", versions: []int{2}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, err := Open(isolatedMySQLConfig(t, "old_history"))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer func() { _ = db.Close() }()

			ctx := context.Background()
			if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (
				version BIGINT NOT NULL PRIMARY KEY,
				applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
			)`); err != nil {
				t.Fatalf("create schema_migrations: %v", err)
			}
			for _, version := range tt.versions {
				if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES (?)`, version); err != nil {
					t.Fatalf("insert pre-release version %d: %v", version, err)
				}
			}

			err = CheckMigrationVersion(ctx, db)
			if err == nil || !strings.Contains(err.Error(), "recreate this pre-release database") {
				t.Fatalf("CheckMigrationVersion() error = %v, want pre-release recreation guidance", err)
			}
		})
	}
}

func TestCheckMigrationVersionReportsUnavailableState(t *testing.T) {
	db, err := Open(isolatedMySQLConfig(t, "missing_history"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	err = CheckMigrationVersion(ctx, db)
	if err == nil || !strings.Contains(err.Error(), "schema migration state unavailable") {
		t.Fatalf("CheckMigrationVersion() error = %v, want unavailable-state context", err)
	}
}

func isolatedMySQLConfig(t *testing.T, suffix string) DBConfig {
	t.Helper()

	parsed, err := mysql.ParseDSN(mysqlDSN(t))
	if err != nil {
		t.Fatalf("parse MYSQL_DSN: %v", err)
	}

	adminCfg := *parsed
	adminCfg.DBName = ""
	admin, err := sqlx.Connect("mysql", adminCfg.FormatDSN())
	if err != nil {
		t.Fatalf("connect without database: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	database := fmt.Sprintf("llm_gateway_%s_%d", suffix, time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE `" + database + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("create isolated migration database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP DATABASE `" + database + "`"); err != nil {
			t.Errorf("drop isolated migration database: %v", err)
		}
	})

	parsed.DBName = database

	return DBConfig{Driver: DriverMySQL, DSN: parsed.FormatDSN()}
}

func TestMigrate_ConcurrentReplicas(t *testing.T) {
	dsn := mysqlDSN(t)
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse MYSQL_DSN: %v", err)
	}

	adminCfg := *parsed
	adminCfg.DBName = ""
	admin, err := sqlx.Connect("mysql", adminCfg.FormatDSN())
	if err != nil {
		t.Fatalf("connect without database: %v", err)
	}
	defer func() { _ = admin.Close() }()

	database := fmt.Sprintf("llm_gateway_migrate_%d", time.Now().UnixNano())
	if _, err = admin.Exec("CREATE DATABASE `" + database + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("create isolated migration database: %v", err)
	}
	defer func() {
		if _, dropErr := admin.Exec("DROP DATABASE `" + database + "`"); dropErr != nil {
			t.Errorf("drop isolated migration database: %v", dropErr)
		}
	}()

	replicaCfg := *parsed
	replicaCfg.DBName = database
	const replicas = 4
	dbs := make([]*sqlx.DB, 0, replicas)
	for range replicas {
		db, openErr := Open(DBConfig{Driver: DriverMySQL, DSN: replicaCfg.FormatDSN()})
		if openErr != nil {
			t.Fatalf("open replica database: %v", openErr)
		}
		dbs = append(dbs, db)
	}
	defer func() {
		for _, db := range dbs {
			_ = db.Close()
		}
	}()

	start := make(chan struct{})
	errs := make(chan error, replicas)
	var wg sync.WaitGroup
	for _, db := range dbs {
		wg.Add(1)
		go func(db *sqlx.DB) {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			errs <- Migrate(ctx, db)
		}(db)
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Migrate: %v", err)
		}
	}
	if t.Failed() {
		return
	}

	if err = CheckMigrationVersion(context.Background(), dbs[0]); err != nil {
		t.Fatalf("CheckMigrationVersion: %v", err)
	}
	var versions int
	if err = dbs[0].Get(&versions, `SELECT COUNT(*) FROM schema_migrations`); err != nil {
		t.Fatalf("count schema migrations: %v", err)
	}
	if versions != latestSchemaVersion {
		t.Fatalf("schema_migrations rows = %d, want %d", versions, latestSchemaVersion)
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

	// Clear before the test to avoid unique constraint conflicts
	// MySQL refuses TRUNCATE on a parent table when FKs reference it
	if _, err := db.Exec(`SET FOREIGN_KEY_CHECKS = 0`); err != nil {
		t.Fatalf("disable FK checks: %v", err)
	}
	for _, table := range []string{
		"policy_bindings",
		"policy_definitions",
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
	// seed default account so subsequent FK inserts pass
	if _, err := db.Exec(`INSERT INTO accounts (pin, name) VALUES ('default', 'Default')`); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	// model_services: trimmed-down columns (v0.3 removed account_id/group_name/spec_detail)
	_, err = db.Exec(
		`INSERT INTO model_services (service_id, model) VALUES (?, ?)`,
		"openai/gpt-4o", "gpt-4o",
	)
	if err != nil {
		t.Fatalf("insert model_services: %v", err)
	}

	// endpoints: the auth column stores an arbitrary VARCHAR string (schema
	// validation is not done at the infra layer; real encryption goes
	// through internal/repo Scanner/Valuer); routing must be valid JSON
	_, err = db.Exec(
		`INSERT INTO endpoints (name, vendor, protocol, model, group_name, weight, enabled, auth, routing)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"openai_main", "openai", "openai", "gpt-4o", "default", 100, true,
		"v1:dummy-ciphertext", `{"url":"https://api.openai.com"}`,
	)
	if err != nil {
		t.Fatalf("insert endpoints: %v", err)
	}

	// api_keys: hash + prefix form
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
