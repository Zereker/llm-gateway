// Package console is the control plane (Admin API) — it replaces the data
// plane's "maintain business data via direct SQL" approach with a governed
// management interface: CRUD + pre-write validation + key issuance + KEK
// encryption + admin auth.
//
// **Architecture position**: a standalone binary (cmd/console), decoupled
// from the data plane (cmd/gateway) **solely through MySQL** — the control
// plane writes, the data plane reads via its TTL cache, and the two never
// call each other synchronously. They share pkg/domain, pkg/repo (including
// Scanner/Valuer + KEK encryption), pkg/infra, and pkg/endpointcheck, which
// keeps the hard contracts that would silently break on drift — schema /
// encryption format / key hash — permanently in sync (the core benefit of
// the monorepo).
package console

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/zereker/llm-gateway/pkg/infra"
)

// Config is the root of console.yaml. The control plane's configuration
// surface is narrow: server / DB / KEK / admin tokens.
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database infra.DBConfig `yaml:"database"`
	// Redis is optional: when configured, revocation goes through cachebus
	// for precise invalidation (notifying the data plane in sub-second time);
	// when not configured it falls back to plain TTL (the data plane expires
	// naturally after <=30s). An empty addr means unconfigured.
	Redis infra.RedisConfig `yaml:"redis"`
	Admin AdminConfig       `yaml:"admin"`

	// DataKey is the AES-256-GCM KEK (64 hex characters). The control plane
	// uses it to encrypt endpoints.auth on write — **it must exactly match
	// the data plane's cfg.data_key**, otherwise the data plane can't decrypt
	// the credentials. Injected from a secret manager in production; never
	// committed.
	DataKey string `yaml:"data_key"`
}

// ServerConfig is the control-plane HTTP server.
type ServerConfig struct {
	Addr              string        `yaml:"addr"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
}

// AdminConfig is static bearer-token auth + role (the RBAC primitive for
// Phase 4; real OIDC/multi-tenancy is left for later). Each token in the yaml
// carries a role (admin / viewer; empty = admin).
type AdminConfig struct {
	Tokens []Token `yaml:"tokens"`
}

// Load reads the YAML, applies env overrides and defaults, and validates.
//
// Env overrides (production injects sensitive fields from a secret manager):
//
//	LLM_GATEWAY_DATABASE_DSN     → database.dsn
//	LLM_GATEWAY_DATA_KEY         → data_key
//	LLM_GATEWAY_CONSOLE_TOKENS   → admin.tokens (comma-separated)
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
		// Env shorthand form: comma-separated bare tokens, all assigned the admin role.
		var toks []Token
		for _, t := range splitComma(v) {
			toks = append(toks, Token{Value: t, Role: RoleAdmin})
		}
		c.Admin.Tokens = toks
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
	for i, t := range c.Admin.Tokens {
		if t.Value == "" {
			return fmt.Errorf("console: admin.tokens[%d].token is empty", i)
		}
		if t.Role == "" {
			c.Admin.Tokens[i].Role = RoleAdmin // default to admin
		} else if t.Role != RoleAdmin && t.Role != RoleViewer {
			return fmt.Errorf("console: admin.tokens[%d].role %q invalid (want admin|viewer)", i, t.Role)
		}
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
