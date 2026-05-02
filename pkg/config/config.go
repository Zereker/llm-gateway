// Package config 加载网关启动配置（gateway.yaml）。
//
// 区分于 pkg/repo —— pkg/repo 是 admin 可增删改的"业务记录"
// （ModelService / Endpoint）；pkg/config 是启动时一次性读入的"网关进程本身的设置"
// （监听端口、超时、apikeys 文件、DB 连接、日志路径等）。
//
// 示例 gateway.yaml 见 configs/local/gateway.yaml。
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 是 gateway.yaml 的根。
//
// 缺省字段会被 ApplyDefaults 填上合理默认值；用户的 YAML 可只声明需要 override 的字段。
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Middleware MiddlewareConfig `yaml:"middleware"`
	Paths      PathsConfig      `yaml:"paths"`
	Database   DatabaseConfig   `yaml:"database"`
}

// ServerConfig HTTP 服务器层配置。
type ServerConfig struct {
	Addr              string        `yaml:"addr"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
}

// MiddlewareConfig 主 middleware 链上的全局参数。
type MiddlewareConfig struct {
	BodyLimitBytes int64         `yaml:"body_limit_bytes"`
	Timeout        time.Duration `yaml:"timeout"`
}

// PathsConfig 文件型数据路径（apikeys 仍是文件，usage 仍是文件追加）。
//
// ModelService / Endpoint 已迁到 DB；不再需要 KV 根目录。
type PathsConfig struct {
	APIKeys  string `yaml:"apikeys"`   // map[apiKey]UserIdentity 的 JSON 文件
	UsageLog string `yaml:"usage_log"` // pkg/usage.FileOutbox 输出文件
}

// DatabaseConfig 业务记录的存储层（ModelService / Endpoint）。
//
//	driver: sqlite | postgres
//	dsn:    sqlite → 文件路径（相对路径相对 yaml 目录）或 ":memory:"
//	        postgres → "postgres://user:pass@host:5432/db?sslmode=disable"
type DatabaseConfig struct {
	Driver string `yaml:"driver"`
	DSN    string `yaml:"dsn"`
}

// Load 从 YAML 文件读入 Config，应用默认值，并把相对路径解析为
// "相对 yaml 文件所在目录"。这样目录可整体迁移：
//
//	configs/local/gateway.yaml 里写 "apikeys: apikeys.json"
//	→ 实际指向 configs/local/apikeys.json，与 CWD 无关
//
// UsageLog 通常是绝对路径（/tmp/... 或 /var/log/...），不做解析以免误把
// /tmp/foo 解释成 configs/local/tmp/foo。
//
// Database.DSN 仅 sqlite 文件路径会做相对解析；":memory:" 与 postgres URL
// 原样保留。
func Load(path string) (*Config, error) {
	if path == "" {
		return nil, errors.New("config: empty path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %q: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("config: parse %q: %w", path, err)
	}
	c.ApplyDefaults()

	base := filepath.Dir(path)
	c.Paths.APIKeys = resolveRelative(base, c.Paths.APIKeys)
	// UsageLog 不解析（约定绝对路径）
	c.Database.DSN = resolveDatabaseDSN(base, c.Database.Driver, c.Database.DSN)

	return &c, nil
}

func resolveRelative(base, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

// resolveDatabaseDSN 仅对 sqlite 的文件路径做相对解析；":memory:" 和 postgres URL 原样返回。
func resolveDatabaseDSN(base, driver, dsn string) string {
	if dsn == "" || dsn == ":memory:" {
		return dsn
	}
	if driver != "sqlite" {
		return dsn // postgres URL / 其它 dialect URL 不动
	}
	// sqlite driver 也可能传 URL 形态（"file:..."），原样保留
	if strings.Contains(dsn, "://") || strings.HasPrefix(dsn, "file:") {
		return dsn
	}
	return resolveRelative(base, dsn)
}

// ApplyDefaults 给所有未设置的字段填默认值。
//
// 调用方可以 zero-value 构造 Config 然后 ApplyDefaults，得到一份"可直接用"的配置。
func (c *Config) ApplyDefaults() {
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}
	if c.Server.ReadHeaderTimeout == 0 {
		c.Server.ReadHeaderTimeout = 10 * time.Second
	}
	if c.Server.ShutdownTimeout == 0 {
		c.Server.ShutdownTimeout = 30 * time.Second
	}
	if c.Middleware.BodyLimitBytes == 0 {
		c.Middleware.BodyLimitBytes = 10 << 20 // 10 MiB
	}
	if c.Middleware.Timeout == 0 {
		c.Middleware.Timeout = 60 * time.Second
	}
	if c.Paths.APIKeys == "" {
		c.Paths.APIKeys = "apikeys.json"
	}
	if c.Paths.UsageLog == "" {
		c.Paths.UsageLog = "/tmp/ai-gateway-usage.log"
	}
	if c.Database.Driver == "" {
		c.Database.Driver = "sqlite"
	}
	if c.Database.DSN == "" {
		c.Database.DSN = "gateway.db"
	}
}
