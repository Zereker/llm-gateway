package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zereker/llm-gateway/internal/infra"
)

func TestLoad_AppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(p, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":8080" {
		t.Errorf("Server.Addr = %q", cfg.Server.Addr)
	}
	if cfg.Request.BodyLimitBytes != 10<<20 {
		t.Errorf("BodyLimitBytes = %d", cfg.Request.BodyLimitBytes)
	}
	if cfg.Request.Timeout != 60*time.Second {
		t.Errorf("Timeout = %v", cfg.Request.Timeout)
	}
	if cfg.Server.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v", cfg.Server.ReadHeaderTimeout)
	}
	if cfg.Database.Driver != infra.DriverMySQL {
		t.Errorf("Database.Driver = %q, want mysql", cfg.Database.Driver)
	}
	if cfg.Database.DSN == "" {
		t.Error("Database.DSN empty after defaults")
	}
}

func TestLoad_HonorsYAMLValues(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gateway.yaml")
	yamlBody := `
server:
  addr: ":9090"
  shutdown_timeout: 5s
request:
  body_limit_bytes: 1048576
  timeout: 30s
database:
  driver: mysql
  dsn: user:pwd@tcp(db.example.com:3306)/prod?parseTime=true
usage_events:
  driver: kafka
  kafka:
    brokers: ["broker1:9092","broker2:9092"]
    topic: billing.usage.recorded.v1
`
	if err := os.WriteFile(p, []byte(yamlBody), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":9090" {
		t.Errorf("Server.Addr = %q", cfg.Server.Addr)
	}
	if cfg.Server.ShutdownTimeout != 5*time.Second {
		t.Errorf("Server.ShutdownTimeout = %v", cfg.Server.ShutdownTimeout)
	}
	if cfg.Request.BodyLimitBytes != 1<<20 {
		t.Errorf("BodyLimitBytes = %d", cfg.Request.BodyLimitBytes)
	}
	if cfg.Request.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v", cfg.Request.Timeout)
	}
	if cfg.Database.Driver != infra.DriverMySQL {
		t.Errorf("Database.Driver = %q", cfg.Database.Driver)
	}
	// The MySQL DSN is a connection string and should not be resolved as a relative path
	if cfg.Database.DSN != "user:pwd@tcp(db.example.com:3306)/prod?parseTime=true" {
		t.Errorf("Database.DSN was rewritten unexpectedly: %q", cfg.Database.DSN)
	}
	if cfg.UsageEvents.Driver != "kafka" {
		t.Errorf("UsageEvents.Driver = %q", cfg.UsageEvents.Driver)
	}
	if len(cfg.UsageEvents.Kafka.Brokers) != 2 || cfg.UsageEvents.Kafka.Topic != "billing.usage.recorded.v1" {
		t.Errorf("UsageEvents.Kafka = %+v", cfg.UsageEvents.Kafka)
	}
}

func TestLoad_OutboxDefaultsToFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gateway.yaml")
	_ = os.WriteFile(p, []byte(""), 0o644)

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UsageEvents.Driver != "file" {
		t.Errorf("UsageEvents.Driver = %q, want file", cfg.UsageEvents.Driver)
	}
	if cfg.UsageEvents.File.Path != "/tmp/llm-gateway-usage.log" {
		t.Errorf("UsageEvents.File.Path = %q", cfg.UsageEvents.File.Path)
	}
}

func TestLoad_RejectsEmptyPath(t *testing.T) {
	if _, err := Load(""); err == nil {
		t.Fatal("want error for empty path")
	}
}

func TestLoad_RejectsMissingFile(t *testing.T) {
	if _, err := Load("/nonexistent/gateway.yaml"); err == nil {
		t.Fatal("want error for missing file")
	}
}

func TestLoad_RejectsBadYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.yaml")
	_ = os.WriteFile(p, []byte("server: not-a-mapping"), 0o644)

	if _, err := Load(p); err == nil {
		t.Fatal("want parse error")
	}
}

func TestLoad_RejectsUnknownField(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "unknown.yaml")
	_ = os.WriteFile(p, []byte("selector:\n  max_atempts: 3\n"), 0o644)

	if _, err := Load(p); err == nil {
		t.Fatal("want unknown-field parse error")
	}
}

func TestLoad_AppliesEnvironmentOverrides(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gateway.yaml")
	_ = os.WriteFile(p, nil, 0o644)
	t.Setenv("LLM_GATEWAY_DATABASE_DSN", "env-dsn")
	t.Setenv("LLM_GATEWAY_REDIS_ADDR", "redis.internal:6379")
	t.Setenv("LLM_GATEWAY_DATA_KEY", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	t.Setenv("LLM_GATEWAY_KAFKA_BROKERS", "k1:9092, k2:9092")

	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.DSN != "env-dsn" || cfg.Redis.Addr != "redis.internal:6379" {
		t.Fatalf("env overrides not applied: %+v", cfg)
	}
	if len(cfg.UsageEvents.Kafka.Brokers) != 2 {
		t.Fatalf("brokers = %#v", cfg.UsageEvents.Kafka.Brokers)
	}
}

func TestValidate_RejectsUnknownDrivers(t *testing.T) {
	tests := []struct {
		name string
		set  func(*Config)
	}{
		{"budget", func(c *Config) { c.Budget.Driver = "bogus" }},
		{"moderation", func(c *Config) { c.Moderation.Driver = "bogus" }},
		{"trace", func(c *Config) { c.Trace.Driver = "bogus" }},
		{"scoring", func(c *Config) { c.Scoring.Driver = "bogus" }},
		{"picker", func(c *Config) { c.Selector.Picker = "bogus" }},
		{"filter", func(c *Config) { c.Selector.Filters = []string{"bogus"} }},
		{"usage", func(c *Config) { c.UsageEvents.Driver = "bogus" }},
		{"contentlog", func(c *Config) { c.ContentLog.Driver = "bogus" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c Config
			c.ApplyDefaults()
			tt.set(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("want validation error")
			}
		})
	}
}

func TestBundledGatewayConfigsParseStrictly(t *testing.T) {
	for _, path := range []string{
		"../../examples/local/configs/gateway.yaml",
		"../../deploy/configs/gateway.yaml",
		"../../examples/full-config/gateway.yaml",
		"../../examples/quickstart/configs/gateway.yaml",
		"../../examples/benchmark/config/gateway.yaml",
	} {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) {
			if _, err := Load(path); err != nil {
				t.Fatalf("Load(%s): %v", path, err)
			}
		})
	}
}

func TestApplyDefaults_OnZeroConfig(t *testing.T) {
	var c Config
	c.ApplyDefaults()
	if c.Server.Addr == "" {
		t.Error("Server.Addr empty after ApplyDefaults")
	}
	if c.Request.BodyLimitBytes == 0 {
		t.Error("BodyLimitBytes zero after ApplyDefaults")
	}
	if c.Database.Driver != infra.DriverMySQL {
		t.Errorf("Database.Driver = %q, want mysql", c.Database.Driver)
	}
}

func TestValidate_RateLimitDriver(t *testing.T) {
	var c Config
	c.ApplyDefaults()

	if c.RateLimit.Driver != "redis" {
		t.Fatalf("default rate_limit.driver = %q, want redis", c.RateLimit.Driver)
	}

	c.RateLimit.Driver = "inmemory"
	if err := c.Validate(); err != nil {
		t.Fatalf("inmemory should validate: %v", err)
	}

	c.RateLimit.Driver = "memcached"
	if err := c.Validate(); err == nil {
		t.Fatal("unknown rate_limit.driver must be rejected")
	}
}

func TestValidate_VendorsOpenAICompatible(t *testing.T) {
	base := func() Config {
		var c Config
		c.ApplyDefaults()

		return c
	}

	c := base()
	c.Vendors.OpenAICompatible = []string{"acme-llm", "foo"}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid vendor names should pass: %v", err)
	}

	for name, bad := range map[string][]string{
		"duplicate":  {"acme-llm", "acme-llm"},
		"whitespace": {"has space"},
		"empty":      {""},
	} {
		c := base()
		c.Vendors.OpenAICompatible = bad
		if err := c.Validate(); err == nil {
			t.Fatalf("%s vendor names must be rejected: %v", name, bad)
		}
	}
}
