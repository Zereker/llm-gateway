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
	"github.com/zereker-labs/ai-gateway/pkg/repo"
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
	engine, cleanup, err := buildEngine(cfg)
	if err != nil {
		t.Fatalf("buildEngine: %v", err)
	}
	defer cleanup()

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

	usageLog, _ := os.ReadFile(cfg.Paths.UsageLog)
	if !strings.Contains(string(usageLog), `"Total":15`) {
		t.Errorf("usage log missing Total:15; got: %s", usageLog)
	}
}

func TestE2E_RejectsMissingAuth(t *testing.T) {
	cfg := writeTestConfig(t, "http://example.invalid")
	engine, cleanup, err := buildEngine(cfg)
	if err != nil {
		t.Fatalf("buildEngine: %v", err)
	}
	defer cleanup()

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestE2E_HealthEndpoints(t *testing.T) {
	cfg := writeTestConfig(t, "http://example.invalid")
	engine, cleanup, err := buildEngine(cfg)
	if err != nil {
		t.Fatalf("buildEngine: %v", err)
	}
	defer cleanup()

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
	engine, cleanup, err := buildEngine(cfg)
	if err != nil {
		t.Fatalf("buildEngine: %v", err)
	}
	defer cleanup()

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"nonexistent-model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer sk-test-alice")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != 404 {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// writeTestConfig 在 t.TempDir() 准备好 apikeys.json + 一个独立 sqlite 文件
// （Migrate + 注入 svc_gpt4o + openai_main），返回 cfg 让 buildEngine 自己再开一次连接。
//
// SQLite 支持同时多连接读同一文件；测试结束 t.TempDir 自动清理。
func writeTestConfig(t *testing.T, upstreamURL string) *config.Config {
	t.Helper()
	dir := t.TempDir()

	apikeys := map[string]domain.UserIdentity{
		"sk-test-alice": {UserID: "alice", APIKeyID: "ak_alice", Group: "default"},
	}
	mustWriteJSON(t, filepath.Join(dir, "apikeys.json"), apikeys)

	dbPath := filepath.Join(dir, "test.db")
	seedDB(t, dbPath, upstreamURL)

	cfg := &config.Config{
		Paths: config.PathsConfig{
			APIKeys:  filepath.Join(dir, "apikeys.json"),
			UsageLog: filepath.Join(dir, "usage.log"),
		},
		Database: config.DatabaseConfig{
			Driver: "sqlite",
			DSN:    dbPath,
		},
		Middleware: config.MiddlewareConfig{
			BodyLimitBytes: 10 << 20,
			Timeout:        5 * time.Second,
		},
	}
	cfg.ApplyDefaults()
	return cfg
}

// seedDB 打开 sqlite 文件、Migrate、写一组测试用 ModelService + Endpoint，然后关闭。
// buildEngine 之后会重新打开同一个文件，读到我们写入的数据。
func seedDB(t *testing.T, dbPath, upstreamURL string) {
	t.Helper()
	db, err := infra.Open(infra.DriverSQLite, dbPath)
	if err != nil {
		t.Fatalf("infra.Open seed: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	if err := infra.Migrate(ctx, db); err != nil {
		t.Fatalf("infra.Migrate seed: %v", err)
	}

	msRepo := repo.NewSQLModelServiceRepo(db)
	if err := msRepo.Create(ctx, &domain.ModelServiceSnapshot{
		ServiceID: "openai/gpt-4o",
		Model:     "gpt-4o",
		Group:     "default",
	}); err != nil {
		t.Fatalf("seed model_service: %v", err)
	}

	epRepo := repo.NewSQLEndpointRepo(db)
	if err := epRepo.Create(ctx, &domain.Endpoint{
		ID:     "openai_main",
		Vendor: "openai",
		URL:    upstreamURL,
		APIKey: domain.Secret("sk-upstream-key"),
		Group:  "default",
		Model:  "gpt-4o",
	}); err != nil {
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
