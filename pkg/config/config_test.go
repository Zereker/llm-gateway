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
  kv_root: /etc/x/kv
  usage_log: /var/log/x.log
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

func TestApplyDefaults_OnZeroConfig(t *testing.T) {
	var c Config
	c.ApplyDefaults()
	if c.Server.Addr == "" {
		t.Error("Server.Addr empty after ApplyDefaults")
	}
	if c.Middleware.BodyLimitBytes == 0 {
		t.Error("BodyLimitBytes zero after ApplyDefaults")
	}
}
