// Package infra is the infrastructure layer: it gathers the code for "how to
// connect to external systems" (SQL / kafka / future redis / s3 / otel /
// etc.), organized by file rather than by sub-package — a sub-package is
// split off only when naming collisions appear or a single package's
// dependency graph gets bloated.
//
// Boundary rule — this package only knows "how to connect," with zero
// knowledge of business entities:
//   - Business tables / entities (ModelService / Endpoint) -> internal/repo
//   - Business query / CRUD                                -> internal/repo
//
// **Each infra subsystem defines its own Config struct** (DBConfig /
// KafkaConfig / ...); internal/config references these types instead of
// redefining them. That way adding a new infra barely touches internal/config,
// and ownership of schema evolution stays concentrated in infra.
//
// The application layer calls Open + Migrate once in main and shares a
// single *sqlx.DB within the process; internal/repo's SQL implementation takes
// *sqlx.DB as a dependency and does not open its own connection.
//
// **v0.1 only supports MySQL 8.0+**. Other dialects such as Postgres /
// SQLite will be supported later by adding a new driver constant plus a
// new schema_<dialect>.sql file; not done for now.
package infra

import (
	"context"
	"embed"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql" // driver name "mysql"
	"github.com/jmoiron/sqlx"
)

// Driver identifies the SQL driver currently in use.
//
// v0.1 only supports MySQL; extending to Postgres etc. later only needs
// a new constant here plus a new schema file.
type Driver string

const (
	DriverMySQL Driver = "mysql"
)

// DBConfig is the SQL database connection configuration.
//
// internal/config exposes these fields to yaml by referencing this type; the
// user's yaml writes:
//
//	database:
//	  driver: mysql
//	  dsn: root:@tcp(localhost:3306)/llm_gateway?parseTime=true&charset=utf8mb4
//
// which lands directly on the *Config.Database field. The DSN must carry
// `parseTime=true`, otherwise reading time.Time fields will error.
type DBConfig struct {
	Driver      Driver `yaml:"driver"`
	DSN         string `yaml:"dsn"`
	AutoMigrate bool   `yaml:"auto_migrate"`
}

// Open opens a *sqlx.DB per cfg and verifies it with a ping.
//
// The application layer calls this once in main; the whole process
// shares one connection pool. The caller is responsible for
// deferring db.Close().
func Open(cfg DBConfig) (*sqlx.DB, error) {
	db, err := sqlx.Open(string(cfg.Driver), cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("infra: open %s: %w", cfg.Driver, err)
	}

	// Reasonable connection pool defaults; the caller can override if needed
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("infra: ping %s: %w", cfg.Driver, err)
	}

	return db, nil
}

//go:embed schema.sql
var schemaFS embed.FS

const latestSchemaVersion = 2

// Migrate applies pending, versioned schema migrations. Production should run
// this through cmd/migrate (or a deployment Job); gateway auto-migration is a
// local-development compatibility option only.
//
// schema.sql is written entirely with IF NOT EXISTS / DEFAULT so it can
// be run repeatedly; a single call at boot is enough.
//
// The MySQL go-sql-driver does not allow multiple statements in a single
// Exec by default (multiStatements=false), so we strip line comments first,
// then split on `;` and execute each statement one at a time; schema.sql
// must not use constructs like "a string literal containing ;".
//
// v0.1 does not introduce golang-migrate / goose; upgrade to those once
// schema evolution needs real versioning (multi-version rollout, rollback
// support, schema shared across services).
func Migrate(ctx context.Context, db *sqlx.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version BIGINT NOT NULL PRIMARY KEY,
		applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return fmt.Errorf("infra: create schema_migrations: %w", err)
	}

	applied := make(map[int]bool)

	var versions []int
	if err := db.SelectContext(ctx, &versions, `SELECT version FROM schema_migrations`); err != nil {
		return fmt.Errorf("infra: read schema migrations: %w", err)
	}

	for _, version := range versions {
		applied[version] = true
	}

	if !applied[1] {
		if err := applyBaseSchema(ctx, db); err != nil {
			return err
		}

		if err := recordMigration(ctx, db, 1); err != nil {
			return err
		}
	}

	if !applied[2] {
		if err := ensureColumn(ctx, db, columnMigration{
			"endpoints", "quirks", "ALTER TABLE endpoints ADD COLUMN quirks JSON DEFAULT NULL",
		}); err != nil {
			return err
		}

		if err := recordMigration(ctx, db, 2); err != nil {
			return err
		}
	}

	return nil
}

func applyBaseSchema(ctx context.Context, db *sqlx.DB) error {
	raw, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("infra: read schema: %w", err)
	}

	for _, stmt := range splitSQL(string(raw)) {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("infra: apply schema: %w\n--- stmt ---\n%s", err, stmt)
		}
	}

	return nil
}

func recordMigration(ctx context.Context, db *sqlx.DB, version int) error {
	if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES (?)`, version); err != nil {
		return fmt.Errorf("infra: record schema migration %d: %w", version, err)
	}

	return nil
}

// CheckMigrationVersion ensures the database was migrated before the service
// starts. It deliberately performs no DDL.
func CheckMigrationVersion(ctx context.Context, db *sqlx.DB) error {
	var version int
	if err := db.GetContext(ctx, &version, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`); err != nil {
		return fmt.Errorf("infra: schema migration state unavailable; run the migrate binary (cmd/migrate): %w", err)
	}
	// Require the DB to be at least this binary's version; a NEWER DB is fine.
	// Migrations are additive (expand-contract: new tables/columns via
	// IF NOT EXISTS / ensureColumn, old fields kept), so an old gateway replica
	// runs correctly against a schema migrated ahead of it. This is what makes
	// zero-downtime rolling upgrades possible: migrate to vN+1 first, old vN
	// pods keep serving, new vN+1 pods roll in — neither crashes.
	if version < latestSchemaVersion {
		return fmt.Errorf("infra: schema version %d is older than required %d; run the migrate binary (cmd/migrate)", version, latestSchemaVersion)
	}

	return nil
}

// columnMigration is a single "add the column if the table is missing it" migration.
type columnMigration struct {
	table, column, ddl string
}

// ensureColumn runs the DDL when the column doesn't exist yet; a no-op otherwise.
//
// **Multi-replica race**: if two replicas both determine the column is
// missing and both ALTER, the one that lands second gets "Duplicate
// column name" (MySQL errno 1060) — at that point the column is already
// in place, so we treat it as success.
func ensureColumn(ctx context.Context, db *sqlx.DB, m columnMigration) error {
	var n int

	err := db.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM information_schema.columns
		 WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`,
		m.table, m.column)
	if err != nil {
		return fmt.Errorf("infra: check column %s.%s: %w", m.table, m.column, err)
	}

	if n > 0 {
		return nil
	}

	if _, err := db.ExecContext(ctx, m.ddl); err != nil {
		if strings.Contains(err.Error(), "Duplicate column name") {
			return nil // a concurrent replica added it first; the target state is already reached
		}

		return fmt.Errorf("infra: add column %s.%s: %w", m.table, m.column, err)
	}

	return nil
}

// splitSQL splits statements on ; and filters out whitespace-only / comment-only
// lines. Simple implementation: it does not parse string literals, so
// schema.sql must not contain a string literal that includes ;.
// splitSQL strips line comments first and only then splits on `;`, so a
// semicolon inside a comment is never mistaken for a statement separator.
func splitSQL(raw string) []string {
	var out []string

	for _, chunk := range strings.Split(stripLineComments(raw), ";") {
		stmt := strings.TrimSpace(chunk)
		if stmt != "" {
			out = append(out, stmt)
		}
	}

	return out
}

// stripLineComments drops `--` comments (whole-line and trailing) and blank
// lines, keeping everything else verbatim.
func stripLineComments(s string) string {
	var keep []string

	for _, line := range strings.Split(s, "\n") {
		line = cutLineComment(line)
		if strings.TrimSpace(line) == "" {
			continue
		}

		keep = append(keep, line)
	}

	return strings.Join(keep, "\n")
}

// cutLineComment removes a trailing `--` comment from one line, ignoring `--`
// that appears inside a single-quoted string literal.
func cutLineComment(line string) string {
	inQuote := false

	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\'':
			inQuote = !inQuote
		case !inQuote && line[i] == '-' && i+1 < len(line) && line[i+1] == '-':
			return line[:i]
		}
	}

	return line
}
