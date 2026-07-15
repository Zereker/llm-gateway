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
	Driver Driver `yaml:"driver"`
	DSN    string `yaml:"dsn"`
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

// Migration files are append-only. Once merged, an existing file must never be
// edited or deleted; schema evolution adds the next numbered file.
//
//go:embed migrations/*.sql
var migrationFS embed.FS

const latestSchemaVersion = 1

type schemaMigration struct {
	version int
	file    string
}

var schemaMigrations = []schemaMigration{
	{version: 1, file: "migrations/000001_base.sql"},
}

// Migrate applies pending, versioned schema migrations during gateway startup.
//
// The MySQL go-sql-driver does not allow multiple statements in a single
// Exec by default (multiStatements=false), so we strip line comments first,
// then split on `;` and execute each statement one at a time; migration files
// must not use constructs like "a string literal containing ;".
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

	for _, migration := range schemaMigrations {
		if applied[migration.version] {
			continue
		}

		if err := applyMigration(ctx, db, migration); err != nil {
			return fmt.Errorf("infra: apply schema migration %d: %w", migration.version, err)
		}

		if err := recordMigration(ctx, db, migration.version); err != nil {
			return err
		}
	}

	return nil
}

func applyMigration(ctx context.Context, db *sqlx.DB, migration schemaMigration) error {
	raw, err := migrationFS.ReadFile(migration.file)
	if err != nil {
		return fmt.Errorf("read %s: %w", migration.file, err)
	}

	for _, stmt := range splitSQL(string(raw)) {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("execute %s: %w\n--- stmt ---\n%s", migration.file, err, stmt)
		}
	}

	return nil
}

func recordMigration(ctx context.Context, db *sqlx.DB, version int) error {
	if _, err := db.ExecContext(ctx, `INSERT IGNORE INTO schema_migrations (version) VALUES (?)`, version); err != nil {
		return fmt.Errorf("infra: record schema migration %d: %w", version, err)
	}

	return nil
}

// CheckMigrationVersion ensures the database was migrated before the service
// starts. It deliberately performs no DDL.
func CheckMigrationVersion(ctx context.Context, db *sqlx.DB) error {
	var versions []int
	if err := db.SelectContext(ctx, &versions, `SELECT version FROM schema_migrations ORDER BY version`); err != nil {
		return fmt.Errorf("infra: schema migration state unavailable: %w", err)
	}

	if len(versions) != len(schemaMigrations) {
		return fmt.Errorf("infra: schema migration history %v does not match required versions 1..%d; recreate this pre-release database", versions, latestSchemaVersion)
	}

	for i, version := range versions {
		want := schemaMigrations[i].version
		if version != want {
			return fmt.Errorf("infra: schema migration history %v does not match required versions 1..%d; recreate this pre-release database", versions, latestSchemaVersion)
		}
	}

	return nil
}

// splitSQL splits statements on ; and filters out whitespace-only / comment-only
// lines. Simple implementation: it does not parse string literals, so
// Migration files must not contain a string literal that includes ;.
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
