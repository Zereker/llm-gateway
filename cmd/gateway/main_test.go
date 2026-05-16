package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zereker/llm-gateway/pkg/config"
	"github.com/zereker/llm-gateway/pkg/infra"
	"github.com/zereker/llm-gateway/pkg/repo"
)

// devDataKey 是 e2e tests 用的 AES KEK；TestMain 装载，供 endpoints.auth 加解密。
const devDataKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestMain(m *testing.M) {
	if err := repo.SetDataKey(devDataKey); err != nil {
		panic("e2e tests: SetDataKey: " + err.Error())
	}
	os.Exit(m.Run())
}

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

// writeTestConfig 准备好 outbox 输出路径，把 Database 段指向本地 MySQL（MYSQL_DSN env），
// 然后 seedDB 直接 INSERT 进 model_services / endpoints / api_keys 三张表。
//
// 没设 MYSQL_DSN 直接 t.Skip 整组 e2e 测试。
func writeTestConfig(t *testing.T, upstreamURL string) *config.Config {
	t.Helper()

	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("MYSQL_DSN not set; skipping gateway e2e test")
	}

	dir := t.TempDir()
	seedDB(t, dsn, upstreamURL)

	cfg := &config.Config{
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
		DataKey: devDataKey,
	}
	cfg.ApplyDefaults()
	return cfg
}

// seedDB 连本地 MySQL，Migrate + TRUNCATE + 写测试用 ModelService + Endpoint + APIKey。
//
// **endpoint 的 auth/routing 走 NamedExec**，让 AuthConfig.Value() 加密、
// RoutingConfig.Value() 序列化 JSON——raw SQL 字符串拿不到这层魔法。
//
// **api_key 走 hash**：SHA-256(plaintext) 落 api_key_hash 列，gateway Resolve
// 时入参 hash 后能查到。
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
	// FK 引用时 MySQL 拒 TRUNCATE 父表（pricing_versions → model_services）；
	// 关 FK check 一把扫
	if _, err := db.Exec(`SET FOREIGN_KEY_CHECKS = 0`); err != nil {
		t.Fatalf("disable FK checks: %v", err)
	}
	for _, table := range []string{
		"pricing_versions",
		"account_model_subscriptions",
		"api_keys",
		"endpoints",
		"model_services",
		"accounts",
		"quota_policies",
	} {
		if _, err := db.Exec("TRUNCATE TABLE " + table); err != nil {
			t.Fatalf("TRUNCATE %s: %v", table, err)
		}
	}
	if _, err := db.Exec(`SET FOREIGN_KEY_CHECKS = 1`); err != nil {
		t.Fatalf("re-enable FK checks: %v", err)
	}

	// 必须先有 accounts("default")（FK 锚点；schema.sql seed 已 INSERT IGNORE 但 TRUNCATE 清掉了）
	if _, err := db.ExecContext(ctx,
		`INSERT INTO accounts (pin, name) VALUES (?, ?)`,
		"default", "Default Account",
	); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	// model_services 全局 catalog（无 account_id / group_name / spec_detail）
	res, err := db.ExecContext(ctx,
		`INSERT INTO model_services (service_id, model) VALUES (?, ?)`,
		"openai/gpt-4o", "gpt-4o",
	)
	if err != nil {
		t.Fatalf("seed model_service: %v", err)
	}
	msID, _ := res.LastInsertId()

	// account 必须订阅 model 才能用（不然 M5 → 403）
	if _, err := db.ExecContext(ctx,
		`INSERT INTO account_model_subscriptions (account_id, model_service_id, enabled) VALUES (?, ?, 1)`,
		"default", msID,
	); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}

	// M5 强制要求 active price，否则 503；e2e 必须 seed 价格
	if _, err := db.ExecContext(ctx,
		`INSERT INTO pricing_versions
		 (account_id, model_service_id, rule_class, effective_from, effective_to, rule_json, created_by, notes)
		 VALUES (?, ?, ?, NOW(6), NULL, ?, ?, ?)`,
		"default", msID, "standard",
		`{"unit":"tokens_per_1m","currency":"USD","rates":{"input":5.0,"output":15.0}}`,
		"e2e-seed", "test fixture",
	); err != nil {
		t.Fatalf("seed pricing: %v", err)
	}

	auth, err := repo.EncodePayload(repo.AuthTypeBearer, repo.BearerAuth{APIKey: "sk-upstream-key"})
	if err != nil {
		t.Fatalf("encode bearer: %v", err)
	}
	ep := &repo.Endpoint{
		Name:    "openai_main",
		Vendor:  "openai",
		Model:   "gpt-4o",
		Group:   "default",
		Weight:  100,
		Enabled: true,
		Auth:    auth,
		Routing: repo.RoutingConfig{URL: upstreamURL},
	}
	if _, err := db.NamedExecContext(ctx,
		`INSERT INTO endpoints
		 (name, vendor, model, group_name, weight, enabled, auth, routing, quota, capabilities, extra)
		 VALUES (:name, :vendor, :model, :group_name, :weight, :enabled, :auth, :routing, :quota, :capabilities, :extra)`,
		ep,
	); err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}

	const aliceKey = "sk-test-alice"
	if _, err := db.ExecContext(ctx,
		`INSERT INTO api_keys
		 (account_id, api_key_hash, api_key_prefix, api_key_id, sub_account_id, group_name, external_user, enabled)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"default", repo.HashAPIKey(aliceKey), aliceKey[:12],
		"ak_alice_test", "alice", "default", false, true,
	); err != nil {
		t.Fatalf("seed api_key: %v", err)
	}
}
