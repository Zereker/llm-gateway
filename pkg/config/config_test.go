package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
	if cfg.Middleware.BodyLimitBytes != 10<<20 {
		t.Errorf("BodyLimitBytes = %d", cfg.Middleware.BodyLimitBytes)
	}
	if cfg.Middleware.Timeout != 60*time.Second {
		t.Errorf("Timeout = %v", cfg.Middleware.Timeout)
	}
	if cfg.Server.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v", cfg.Server.ReadHeaderTimeout)
	}
	if cfg.Database.Driver != "sqlite" {
		t.Errorf("Database.Driver = %q, want sqlite", cfg.Database.Driver)
	}
	// gateway.db 是默认值，相对路径 → 解析为相对 yaml 目录
	wantDSN := filepath.Join(dir, "gateway.db")
	if cfg.Database.DSN != wantDSN {
		t.Errorf("Database.DSN = %q, want %q", cfg.Database.DSN, wantDSN)
	}
}

func TestLoad_HonorsYAMLValues(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gateway.yaml")
	yamlBody := `
server:
  addr: ":9090"
  shutdown_timeout: 5s
middleware:
  body_limit_bytes: 1048576
  timeout: 30s
paths:
  apikeys: /etc/x/apikeys.json
database:
  driver: postgres
  dsn: postgres://u:p@localhost:5432/db?sslmode=disable
outbox:
  driver: kafka
  kafka:
    brokers: ["broker1:9092","broker2:9092"]
    topic: ai-gateway.usage
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
	if cfg.Middleware.BodyLimitBytes != 1<<20 {
		t.Errorf("BodyLimitBytes = %d", cfg.Middleware.BodyLimitBytes)
	}
	if cfg.Middleware.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v", cfg.Middleware.Timeout)
	}
	if cfg.Paths.APIKeys != "/etc/x/apikeys.json" {
		t.Errorf("Paths.APIKeys = %q", cfg.Paths.APIKeys)
	}
	if cfg.Database.Driver != "postgres" {
		t.Errorf("Database.Driver = %q", cfg.Database.Driver)
	}
	// postgres URL 不应被相对解析
	if cfg.Database.DSN != "postgres://u:p@localhost:5432/db?sslmode=disable" {
		t.Errorf("Database.DSN was resolved unexpectedly: %q", cfg.Database.DSN)
	}
	if cfg.Outbox.Driver != "kafka" {
		t.Errorf("Outbox.Driver = %q", cfg.Outbox.Driver)
	}
	if len(cfg.Outbox.Kafka.Brokers) != 2 || cfg.Outbox.Kafka.Topic != "ai-gateway.usage" {
		t.Errorf("Outbox.Kafka = %+v", cfg.Outbox.Kafka)
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
	if cfg.Outbox.Driver != "file" {
		t.Errorf("Outbox.Driver = %q, want file", cfg.Outbox.Driver)
	}
	if cfg.Outbox.File.Path != "/tmp/ai-gateway-usage.log" {
		t.Errorf("Outbox.File.Path = %q", cfg.Outbox.File.Path)
	}
}

func TestLoad_SQLiteDSN_RelativeResolved(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gateway.yaml")
	_ = os.WriteFile(p, []byte("database:\n  driver: sqlite\n  dsn: my.db\n"), 0o644)

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := filepath.Join(dir, "my.db")
	if cfg.Database.DSN != want {
		t.Errorf("DSN = %q, want %q", cfg.Database.DSN, want)
	}
}

func TestLoad_SQLiteDSN_MemoryAndAbsolute(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gateway.yaml")
	_ = os.WriteFile(p, []byte("database:\n  driver: sqlite\n  dsn: \":memory:\"\n"), 0o644)

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.DSN != ":memory:" {
		t.Errorf("DSN = %q, want :memory:", cfg.Database.DSN)
	}

	// 绝对路径也不应被改
	_ = os.WriteFile(p, []byte("database:\n  driver: sqlite\n  dsn: /tmp/abs.db\n"), 0o644)
	cfg, _ = Load(p)
	if cfg.Database.DSN != "/tmp/abs.db" {
		t.Errorf("DSN = %q, want /tmp/abs.db", cfg.Database.DSN)
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

func TestLoadAdmin_AppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "admin.yaml")
	_ = os.WriteFile(p, []byte("admin:\n  token: my-token\n"), 0o644)

	cfg, err := LoadAdmin(p)
	if err != nil {
		t.Fatalf("LoadAdmin: %v", err)
	}
	if cfg.Server.Addr != ":8081" {
		t.Errorf("Server.Addr = %q, want :8081", cfg.Server.Addr)
	}
	if cfg.Admin.Token != "my-token" {
		t.Errorf("Admin.Token = %q", cfg.Admin.Token)
	}
	if cfg.Database.Driver != "sqlite" {
		t.Errorf("Database.Driver = %q", cfg.Database.Driver)
	}
	want := filepath.Join(dir, "gateway.db")
	if cfg.Database.DSN != want {
		t.Errorf("Database.DSN = %q, want %q", cfg.Database.DSN, want)
	}
}

func TestLoadAdmin_HonorsYAMLValues(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "admin.yaml")
	_ = os.WriteFile(p, []byte(`
server:
  addr: "127.0.0.1:9999"
admin:
  token: secret-tok
database:
  driver: postgres
  dsn: postgres://u:p@h:5432/db
`), 0o644)

	cfg, err := LoadAdmin(p)
	if err != nil {
		t.Fatalf("LoadAdmin: %v", err)
	}
	if cfg.Server.Addr != "127.0.0.1:9999" {
		t.Errorf("Server.Addr = %q", cfg.Server.Addr)
	}
	if cfg.Admin.Token != "secret-tok" {
		t.Errorf("Admin.Token = %q", cfg.Admin.Token)
	}
	if cfg.Database.DSN != "postgres://u:p@h:5432/db" {
		t.Errorf("Database.DSN = %q (postgres URL should pass through)", cfg.Database.DSN)
	}
}

func TestLoadAdmin_TokenStaysEmptyByDefault(t *testing.T) {
	// Token 没默认；缺失时 adminAuthMW 必须拒绝所有请求。
	dir := t.TempDir()
	p := filepath.Join(dir, "admin.yaml")
	_ = os.WriteFile(p, []byte(""), 0o644)

	cfg, _ := LoadAdmin(p)
	if cfg.Admin.Token != "" {
		t.Errorf("Admin.Token should default to empty, got %q", cfg.Admin.Token)
	}
}

func TestApplyDefaults_OnZeroConfig(t *testing.T) {
	var c Config
	c.ApplyDefaults()
	if c.Server.Addr == "" {
		t.Error("Server.Addr empty after ApplyDefaults")
	}
	if c.Middleware.BodyLimitBytes == 0 {
		t.Error("BodyLimitBytes zero after ApplyDefaults")
	}
	if c.Database.Driver != "sqlite" {
		t.Errorf("Database.Driver = %q, want sqlite", c.Database.Driver)
	}
	if c.Database.DSN != "gateway.db" {
		t.Errorf("Database.DSN = %q, want gateway.db", c.Database.DSN)
	}
}
