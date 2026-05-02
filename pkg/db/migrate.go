package db

import (
	"context"
	"embed"
	"fmt"

	"github.com/jmoiron/sqlx"
)

//go:embed schema.sql
var schemaFS embed.FS

// Migrate 把 embed 的 schema.sql 应用到 db。
//
// schema.sql 全部用 IF NOT EXISTS / DEFAULT 写法，可以反复 Run；
// boot 时调一次即可。
//
// v0.1 不引入 golang-migrate / goose；schema 演进到需要 versioning
// （多版本上线、需要回滚、跨服务共享 schema）时再升级。
func Migrate(ctx context.Context, db *sqlx.DB) error {
	raw, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("db: read schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, string(raw)); err != nil {
		return fmt.Errorf("db: apply schema: %w", err)
	}
	return nil
}
