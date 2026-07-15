package repo

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
)

func TestCheckSchemaReportsTableAndMigrationGuidance(t *testing.T) {
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("MYSQL_DSN not set; skipping CheckSchema integration test")
	}

	db, err := sqlx.Connect("mysql", dsn)
	if err != nil {
		t.Fatalf("connect MySQL: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = CheckSchema(ctx, db)
	if err == nil || !strings.Contains(err.Error(), "quota_policies") || !strings.Contains(err.Error(), "embedded infra migrations") {
		t.Fatalf("CheckSchema() error = %v, want table name and migration guidance", err)
	}
}
