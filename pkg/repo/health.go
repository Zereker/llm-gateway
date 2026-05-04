package repo

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// CheckSchema 验证业务表（model_services / endpoints）已存在且可读。
//
// gateway 启动期调用：表不在就 fail-fast，提示 deployer 先跑 cmd/admin。
// 不读数据，只发 prepare 级别的查询；schema 缺失时返回带表名的明确错误。
//
// 设计考虑：schema 演进的所有权归 admin，gateway 不再调用 infra.Migrate。
// 这个函数让 gateway 在 boot 失败时给出比"SQL: no such table"更友好的提示。
func CheckSchema(ctx context.Context, db *sqlx.DB) error {
	for _, t := range []string{
		"quota_policies",
		"tenants",
		"model_services",
		"endpoints",
		"tenant_model_subscriptions",
		"api_keys",
		"pricing_versions",
	} {
		if _, err := db.ExecContext(ctx, "SELECT 1 FROM "+t+" LIMIT 0"); err != nil {
			return fmt.Errorf("repo: schema check failed on %q (run cmd/admin to bootstrap): %w", t, err)
		}
	}
	return nil
}
