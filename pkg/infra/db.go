// Package infra 是基础设施层：收拢"如何连外部系统"的代码（SQL / kafka /
// 未来的 redis / s3 / otel 等），按 file 而非 sub-package 组织——只在出现命名冲突或单包
// 依赖图臃肿时才拆 sub-package。
//
// 边界规则——本包只知道"怎么连"，零业务实体知识：
//   - 业务表 / 实体（ModelService / Endpoint）→ pkg/repo
//   - 业务 query / CRUD                         → pkg/repo
//
// **每个 infra 子系统自己定义 Config 结构体**（DBConfig / KafkaConfig / ...），
// pkg/config 引用这些类型而不是重新定义；这样新增 infra 时 pkg/config 几乎不变，
// schema 演进的所有权集中在 infra 这边。
//
// 应用层在 main 调一次 Open + Migrate，进程内共享一份 *sqlx.DB；
// pkg/repo 的 SQL 实现接受 *sqlx.DB 作为依赖，自己不打开连接。
//
// **v0.1 只支持 MySQL 8.0+**。Postgres / SQLite 等其它方言以后通过新增 driver
// 常量 + 新 schema_<dialect>.sql 文件支持，目前不做。
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

// Driver 标识当前使用的 SQL 驱动。
//
// v0.1 只支持 MySQL；以后扩展 Postgres 等只需在此加常量 + 新 schema 文件。
type Driver string

const (
	DriverMySQL Driver = "mysql"
)

// DBConfig SQL 数据库连接配置。
//
// pkg/config 通过引用本类型把字段暴露给 yaml；用户 yaml 写：
//
//	database:
//	  driver: mysql
//	  dsn: root:@tcp(localhost:3306)/llm_gateway?parseTime=true&charset=utf8mb4
//
// 直接落到 *Config.Database 字段。DSN 必须带 `parseTime=true`，否则
// time.Time 字段读取会出错。
type DBConfig struct {
	Driver Driver `yaml:"driver"`
	DSN    string `yaml:"dsn"`
}

// Open 按 cfg 打开 *sqlx.DB 并 ping 验证。
//
// 应用层只在 main 调一次，整个进程共享一份连接池。
// 调用方负责 defer db.Close()。
func Open(cfg DBConfig) (*sqlx.DB, error) {
	db, err := sqlx.Open(string(cfg.Driver), cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("infra: open %s: %w", cfg.Driver, err)
	}

	// 合理的连接池默认；需要时调用方覆盖
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

// Migrate 把 embed 的 schema.sql 应用到 db。
//
// schema.sql 全部用 IF NOT EXISTS / DEFAULT 写法，可以反复 Run；
// boot 时调一次即可。
//
// MySQL go-sql-driver 默认不允许单次 Exec 多语句（multiStatements=false），
// 这里按 `;` 拆开逐条执行；schema.sql 不能用"字符串里含 ;"这种构造。
//
// v0.1 不引入 golang-migrate / goose；schema 演进到需要 versioning
// （多版本上线、需要回滚、跨服务共享 schema）时再升级。
func Migrate(ctx context.Context, db *sqlx.DB) error {
	raw, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("infra: read schema: %w", err)
	}
	for _, stmt := range splitSQL(string(raw)) {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("infra: apply schema: %w\n--- stmt ---\n%s", err, stmt)
		}
	}

	// 列级演进：CREATE TABLE IF NOT EXISTS 对已存在的表是 no-op，schema.sql 里
	// 新加的列不会落到老库。MySQL 又不支持 ADD COLUMN IF NOT EXISTS（MariaDB
	// 才有），所以这里用 information_schema 判断后再 ALTER。
	//
	// 新列在这里登记一行；老列稳定后可清理。
	for _, m := range []columnMigration{
		{"endpoints", "quirks", "ALTER TABLE endpoints ADD COLUMN quirks JSON DEFAULT NULL"},
	} {
		if err := ensureColumn(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

// columnMigration 一条"表缺列则补"的迁移。
type columnMigration struct {
	table, column, ddl string
}

// ensureColumn 列不存在时执行 DDL；存在则 no-op。
//
// **多副本竞态**：两个副本同时判断"列不存在"、同时 ALTER 时，后到的会收到
// "Duplicate column name"（MySQL errno 1060）——此时列已就位，视为成功。
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
			return nil // 并发副本抢先加了；目标状态已达成
		}
		return fmt.Errorf("infra: add column %s.%s: %w", m.table, m.column, err)
	}
	return nil
}

// splitSQL 按 ; 切分语句，过滤纯空白 / 纯注释行。简单实现：不解析字符串字面量，
// schema.sql 不允许出现"字符串内含 ;"。
func splitSQL(raw string) []string {
	var out []string
	for _, chunk := range strings.Split(raw, ";") {
		stmt := stripCommentsAndTrim(chunk)
		if stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}

// stripCommentsAndTrim 去掉行注释 + trim 整体空白；剩下空字符串则该语句被跳过。
func stripCommentsAndTrim(s string) string {
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "--") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}
