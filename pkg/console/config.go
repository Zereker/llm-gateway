// Package console 是控制面（Admin API）——把数据面"直接 SQL 维护业务数据"换成
// 受控的管理接口：CRUD + 写前校验 + 发 key + KEK 加密 + admin 鉴权。
//
// **架构定位**：独立 binary（cmd/console），跟数据面（cmd/gateway）**只通过 MySQL
// 解耦**——控制面写、数据面按 TTL 缓存读，两者不同步调用。共享 pkg/domain、
// pkg/repo（含 Scanner/Valuer + KEK 加密）、pkg/infra、pkg/endpointcheck，保证
// schema / 加密格式 / key hash 这些"漂移即静默损坏"的硬契约永远一致（monorepo 的
// 核心收益）。
package console

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/zereker/llm-gateway/pkg/infra"
)

// Config 是 console.yaml 的根。控制面配置面很窄：server / DB / KEK / admin token。
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database infra.DBConfig `yaml:"database"`
	Admin    AdminConfig    `yaml:"admin"`

	// DataKey 是 AES-256-GCM 的 KEK（hex 64 字符）。控制面写 endpoints.auth 时用它
	// 加密——**必须跟数据面 cfg.data_key 完全一致**，否则数据面解不开凭证。
	// 生产从 secret manager 注入，不 commit。
	DataKey string `yaml:"data_key"`
}

// ServerConfig 控制面 HTTP server。
type ServerConfig struct {
	Addr              string        `yaml:"addr"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
}

// AdminConfig Phase 0 用静态 bearer token 鉴权（Phase 4 换真 RBAC / OIDC）。
type AdminConfig struct {
	Tokens []string `yaml:"tokens"`
}

// Load 读 YAML + env 覆盖 + 默认值 + 校验。
//
// env 覆盖（生产从 secret manager 注入敏感字段）：
//
//	LLM_GATEWAY_DATABASE_DSN     → database.dsn
//	LLM_GATEWAY_DATA_KEY         → data_key
//	LLM_GATEWAY_CONSOLE_TOKENS   → admin.tokens（逗号分隔）
func Load(path string) (*Config, error) {
	var cfg Config
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("console: read config: %w", err)
		}
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			return nil, fmt.Errorf("console: parse config: %w", err)
		}
	}
	cfg.applyEnv()
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyEnv() {
	if v := os.Getenv("LLM_GATEWAY_DATABASE_DSN"); v != "" {
		c.Database.DSN = v
	}
	if v := os.Getenv("LLM_GATEWAY_DATA_KEY"); v != "" {
		c.DataKey = v
	}
	if v := os.Getenv("LLM_GATEWAY_CONSOLE_TOKENS"); v != "" {
		c.Admin.Tokens = splitComma(v)
	}
}

func (c *Config) applyDefaults() {
	if c.Server.Addr == "" {
		c.Server.Addr = ":8081"
	}
	if c.Server.ReadHeaderTimeout == 0 {
		c.Server.ReadHeaderTimeout = 10 * time.Second
	}
	if c.Server.ShutdownTimeout == 0 {
		c.Server.ShutdownTimeout = 15 * time.Second
	}
	if c.Database.Driver == "" {
		c.Database.Driver = infra.DriverMySQL
	}
}

func (c *Config) validate() error {
	if c.Database.DSN == "" {
		return fmt.Errorf("console: database.dsn required (set LLM_GATEWAY_DATABASE_DSN or config)")
	}
	if c.DataKey == "" {
		return fmt.Errorf("console: data_key required (must match gateway's KEK)")
	}
	if len(c.Admin.Tokens) == 0 {
		return fmt.Errorf("console: admin.tokens required (at least one bearer token; set LLM_GATEWAY_CONSOLE_TOKENS)")
	}
	return nil
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			tok := s[start:i]
			// trim spaces
			for len(tok) > 0 && tok[0] == ' ' {
				tok = tok[1:]
			}
			for len(tok) > 0 && tok[len(tok)-1] == ' ' {
				tok = tok[:len(tok)-1]
			}
			if tok != "" {
				out = append(out, tok)
			}
			start = i + 1
		}
	}
	return out
}
