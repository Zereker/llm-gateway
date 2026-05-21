package repo

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// CheckSchema 验证业务表已存在且可读——gateway 启动期跑 infra.Migrate 之后的
// 防御性检查（schema.sql 漏改时早暴露，比"SQL: no such table"友好）。
//
// 不读数据，只发 prepare 级别的查询。表数据为空时 gateway 仍能启动，
// 请求过来时 M5 / M7 / M2 各自 404 / 503 / 401（deployer 通过 SQL 直接管理数据）。
func CheckSchema(ctx context.Context, db *sqlx.DB) error {
	for _, t := range []string{
		"quota_policies",
		"accounts",
		"model_services",
		"endpoints",
		"account_model_subscriptions",
		"api_keys",
		"pricing_versions",
	} {
		if _, err := db.ExecContext(ctx, "SELECT 1 FROM "+t+" LIMIT 0"); err != nil {
			return fmt.Errorf("repo: schema check failed on %q (run infra.Migrate or apply schema.sql first): %w", t, err)
		}
	}
	return nil
}
