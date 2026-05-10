package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zereker/llm-gateway/pkg/infra"
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
middleware:
  body_limit_bytes: 1048576
  timeout: 30s
database:
  driver: mysql
  dsn: user:pwd@tcp(db.example.com:3306)/prod?parseTime=true
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
	if cfg.Database.Driver != infra.DriverMySQL {
		t.Errorf("Database.Driver = %q", cfg.Database.Driver)
	}
	// MySQL DSN 是连接字符串，不应被相对解析
	if cfg.Database.DSN != "user:pwd@tcp(db.example.com:3306)/prod?parseTime=true" {
		t.Errorf("Database.DSN was rewritten unexpectedly: %q", cfg.Database.DSN)
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
	if cfg.Database.Driver != infra.DriverMySQL {
		t.Errorf("Database.Driver = %q", cfg.Database.Driver)
	}
	if cfg.Database.DSN == "" {
		t.Error("Database.DSN empty after defaults")
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
  driver: mysql
  dsn: user:pwd@tcp(db.example.com:3306)/prod?parseTime=true
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
	if cfg.Database.DSN != "user:pwd@tcp(db.example.com:3306)/prod?parseTime=true" {
		t.Errorf("Database.DSN was rewritten unexpectedly: %q", cfg.Database.DSN)
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
	if c.Database.Driver != infra.DriverMySQL {
		t.Errorf("Database.Driver = %q, want mysql", c.Database.Driver)
	}
}
