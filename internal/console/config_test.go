package console

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigRejectsUnknownField(t *testing.T) {
	p := filepath.Join(t.TempDir(), "console.yaml")
	if err := os.WriteFile(p, []byte("server:\n  adrr: ':8081'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "field adrr not found") {
		t.Fatalf("Load error = %v", err)
	}
}

func TestLoadConfigAppliesSharedRedisEnvironment(t *testing.T) {
	p := filepath.Join(t.TempDir(), "console.yaml")
	body := "database:\n  dsn: yaml-dsn\ndata_key: " + strings.Repeat("0", 64) + "\nadmin:\n  tokens:\n    - token: test\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LLM_GATEWAY_REDIS_ADDR", "redis.internal:6379")
	t.Setenv("LLM_GATEWAY_REDIS_PASSWORD", "secret")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Redis.Addr != "redis.internal:6379" || cfg.Redis.Password != "secret" {
		t.Fatalf("redis env overrides not applied: %+v", cfg.Redis)
	}
}

func TestLoadConfigRejectsInvalidDataKeyAndDuplicateToken(t *testing.T) {
	tests := map[string]string{
		"non-hex data key": "database:\n  dsn: test\ndata_key: " + "zz" + strings.Repeat("0", 62) + "\nadmin:\n  tokens:\n    - token: one\n",
		"duplicate token":  "database:\n  dsn: test\ndata_key: " + strings.Repeat("0", 64) + "\nadmin:\n  tokens:\n    - token: same\n    - token: same\n",
	}

	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "console.yaml")
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(p); err == nil {
				t.Fatal("want config validation error")
			}
		})
	}
}
