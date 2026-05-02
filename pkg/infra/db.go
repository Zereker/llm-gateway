// Package infra 是基础设施层：收拢"如何连外部系统"的代码（SQL / 未来的 kafka /
// redis / s3 / otel 等），按 file 而非 sub-package 组织——只在出现命名冲突或单包
// 依赖图臃肿时才拆 sub-package。
//
// 边界规则——本包只知道"怎么连"，零业务实体知识：
//   - 业务表 / 实体（ModelService / Endpoint）→ pkg/repo
//   - 业务 query / CRUD                         → pkg/repo
//
// 应用层在 main 调一次 Open + Migrate，进程内共享一份 *sqlx.DB；
// pkg/repo 的 SQL 实现接受 *sqlx.DB 作为依赖，自己不打开连接。
package infra

import (
	"context"
	"embed"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	_ "modernc.org/sqlite" // 纯 Go sqlite driver；driver name "sqlite"
)

// Driver 标识当前使用的 SQL 驱动。
type Driver string

const (
	DriverSQLite   Driver = "sqlite"   // 默认；适合本地 / 单实例 / OSS 零安装
	DriverPostgres Driver = "postgres" // v0.1 占位；正式接入时 import lib/pq
)

// Open 按 driver / dsn 打开 *sqlx.DB 并 ping 验证。
//
// 应用层只在 main 调一次，整个进程共享一份连接池。
// 调用方负责 defer db.Close()。
func Open(driver Driver, dsn string) (*sqlx.DB, error) {
	db, err := sqlx.Open(string(driver), dsn)
	if err != nil {
		return nil, fmt.Errorf("infra: open %s: %w", driver, err)
	}

	// 合理的连接池默认；需要时调用方覆盖
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("infra: ping %s: %w", driver, err)
	}
	return db, nil
}

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
		return fmt.Errorf("infra: read schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, string(raw)); err != nil {
		return fmt.Errorf("infra: apply schema: %w", err)
	}
	return nil
}
