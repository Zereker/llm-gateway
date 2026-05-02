// Package db 是 SQL 基础设施层：负责"如何连 DB"和"建表"，不包含任何业务实体。
//
// 边界——本包只知道：
//   - 怎么按 driver / dsn 打开 *sqlx.DB
//   - 连接池合理默认
//   - 怎么把 embed 的 schema.sql 应用到 DB（idempotent）
//
// 本包不知道：
//   - 业务表 / 实体（ModelService / Endpoint）→ 见 pkg/repo
//   - 业务 query / CRUD                       → 见 pkg/repo
//
// 应用层在 main 调用一次 Open + Migrate，进程内共享同一份 *sqlx.DB；
// pkg/repo 的 SQL 实现接受 *sqlx.DB 作为依赖，自己不打开连接。
package db

import (
	"context"
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
		return nil, fmt.Errorf("db: open %s: %w", driver, err)
	}

	// 合理的连接池默认；需要时调用方覆盖
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("db: ping %s: %w", driver, err)
	}
	return db, nil
}
