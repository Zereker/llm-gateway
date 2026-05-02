package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zereker-labs/ai-gateway/pkg/config"
	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/infra"
)

// e2e: 用 httptest 模拟 OpenAI 上游，把 gateway 的全套 middleware 串起来跑一遍。
func TestE2E_OpenAIChatCompletions(t *testing.T) {
	var capturedAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer upstream.Close()

	cfg := writeTestConfig(t, upstream.URL)
	engine, srv, err := buildEngine(cfg)
	if err != nil {
		t.Fatalf("buildEngine: %v", err)
	}
	defer srv.Close()

	body := `{"model":"gpt-4o","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test-alice")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"content":"hi"`) {
		t.Errorf("body missing content: %s", w.Body.String())
	}
	if capturedAuth != "Bearer sk-upstream-key" {
		t.Errorf("upstream Authorization = %q", capturedAuth)
	}

	usageLog, _ := os.ReadFile(cfg.Outbox.File.Path)
	if !strings.Contains(string(usageLog), `"Total":15`) {
		t.Errorf("usage log missing Total:15; got: %s", usageLog)
	}
}

func TestE2E_RejectsMissingAuth(t *testing.T) {
	cfg := writeTestConfig(t, "http://example.invalid")
	engine, srv, err := buildEngine(cfg)
	if err != nil {
		t.Fatalf("buildEngine: %v", err)
	}
	defer srv.Close()

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestE2E_HealthEndpoints(t *testing.T) {
	cfg := writeTestConfig(t, "http://example.invalid")
	engine, srv, err := buildEngine(cfg)
	if err != nil {
		t.Fatalf("buildEngine: %v", err)
	}
	defer srv.Close()

	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Errorf("%s: status = %d", path, w.Code)
		}
	}
}

func TestE2E_RejectsUnknownModel(t *testing.T) {
	cfg := writeTestConfig(t, "http://example.invalid")
	engine, srv, err := buildEngine(cfg)
	if err != nil {
		t.Fatalf("buildEngine: %v", err)
	}
	defer srv.Close()

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"nonexistent-model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer sk-test-alice")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != 404 {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// writeTestConfig 在 t.TempDir() 准备好 apikeys.json + outbox 输出路径，
// 把 Database 段指向本地 MySQL（MYSQL_DSN env），然后 seedDB 写入测试数据。
//
// 没设 MYSQL_DSN 直接 t.Skip 整组 e2e 测试。
func writeTestConfig(t *testing.T, upstreamURL string) *config.Config {
	t.Helper()

	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("MYSQL_DSN not set; skipping gateway e2e test")
	}

	dir := t.TempDir()

	apikeys := map[string]domain.UserIdentity{
		"sk-test-alice": {UserID: "alice", APIKeyID: "ak_alice", Group: "default"},
	}
	mustWriteJSON(t, filepath.Join(dir, "apikeys.json"), apikeys)

	seedDB(t, dsn, upstreamURL)

	cfg := &config.Config{
		Paths: config.PathsConfig{
			APIKeys: filepath.Join(dir, "apikeys.json"),
		},
		Database: infra.DBConfig{
			Driver: infra.DriverMySQL,
			DSN:    dsn,
		},
		Outbox: config.OutboxConfig{
			Driver: "file",
			File:   config.FileOutboxSection{Path: filepath.Join(dir, "usage.log")},
		},
		Middleware: config.MiddlewareConfig{
			BodyLimitBytes: 10 << 20,
			Timeout:        5 * time.Second,
		},
	}
	cfg.ApplyDefaults()
	return cfg
}

// seedDB 连本地 MySQL，Migrate + TRUNCATE + 写测试用 ModelService + Endpoint。
// buildEngine 后续会再开一次连接，读到这些数据。
func seedDB(t *testing.T, dsn, upstreamURL string) {
	t.Helper()
	db, err := infra.Open(infra.DBConfig{Driver: infra.DriverMySQL, DSN: dsn})
	if err != nil {
		t.Fatalf("infra.Open seed: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	if err := infra.Migrate(ctx, db); err != nil {
		t.Fatalf("infra.Migrate seed: %v", err)
	}
	for _, table := range []string{"endpoints", "model_services"} {
		if _, err := db.Exec("TRUNCATE TABLE " + table); err != nil {
			t.Fatalf("TRUNCATE %s: %v", table, err)
		}
	}

	// 用 raw SQL 直接 INSERT，避开 admin 写路径（gateway 测试不需要 import admin）。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO model_services (service_id, model, group_name) VALUES (?, ?, ?)`,
		"openai/gpt-4o", "gpt-4o", "default",
	); err != nil {
		t.Fatalf("seed model_service: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO endpoints (id, vendor, url, api_key, group_name, model)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"openai_main", "openai", upstreamURL, "sk-upstream-key", "default", "gpt-4o",
	); err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}
}

func mustWriteJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
