package repo

import (
	"context"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/zereker-labs/ai-gateway/pkg/infra"
)

// newTestDB 起一个内存 sqlite 并跑 Migrate；t.Cleanup 自动 Close。
//
// 给 modelservice_sql_test.go / endpoint_sql_test.go 共用。
func newTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := infra.Open(infra.DBConfig{Driver: infra.DriverSQLite, DSN: ":memory:"})
	if err != nil {
		t.Fatalf("infra.Open: %v", err)
	}
	if err := infra.Migrate(context.Background(), db); err != nil {
		_ = db.Close()
		t.Fatalf("infra.Migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
