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

// devDataKey is the AES KEK used by the e2e tests; TestMain loads it for encrypting/decrypting endpoints.auth.
const devDataKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestMain(m *testing.M) {
	if err := repo.SetDataKey(devDataKey); err != nil {
		panic("e2e tests: SetDataKey: " + err.Error())
	}
	os.Exit(m.Run())
}

// e2e: with response cache enabled, a deterministic request (temperature=0,
// non-streaming) hits the cache on the second call—no upstream call, and the
// X-Gateway-Cache: hit header is set. A unique nonce in the body guarantees
// the first call is always a miss.
func TestE2E_ResponseCacheHit(t *testing.T) {
	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer upstream.Close()

	cfg := writeTestConfig(t, upstream.URL)
	cfg.Cache.Enabled = true
	cfg.Cache.TTL = time.Minute
	engine, srv, err := buildEngine(cfg)
	if err != nil {
		t.Fatalf("buildEngine: %v", err)
	}
	defer srv.Close()

	nonce := time.Now().UnixNano()
	body := `{"model":"gpt-4o","stream":false,"temperature":0,"user":"cachetest-` +
		strconvItoa(nonce) + `","messages":[{"role":"user","content":"hi"}]}`
	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer sk-test-alice")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		return w
	}

	w1 := send()
	if w1.Code != 200 {
		t.Fatalf("first = %d, body=%s", w1.Code, w1.Body.String())
	}
	if upstreamCalls != 1 {
		t.Fatalf("first should hit upstream once, got %d", upstreamCalls)
	}
	if w1.Header().Get("X-Gateway-Cache") == "hit" {
		t.Error("first should be a miss, not hit")
	}

	w2 := send()
	if w2.Code != 200 {
		t.Fatalf("second = %d", w2.Code)
	}
	if w2.Header().Get("X-Gateway-Cache") != "hit" {
		t.Errorf("second should be X-Gateway-Cache: hit, got %q", w2.Header().Get("X-Gateway-Cache"))
	}
	if upstreamCalls != 1 {
		t.Errorf("second should NOT hit upstream, upstreamCalls=%d want 1", upstreamCalls)
	}
	if w1.Body.String() != w2.Body.String() {
		t.Errorf("cached body differs:\n1=%s\n2=%s", w1.Body.String(), w2.Body.String())
	}
}

func strconvItoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [24]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// e2e: uses httptest to simulate an OpenAI upstream and runs the gateway's full middleware chain end to end.
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

	usageLog, _ := os.ReadFile(cfg.UsageEvents.File.Path)
	if !strings.Contains(string(usageLog), `"total":15`) {
		t.Errorf("usage log missing total:15; got: %s", usageLog)
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

// writeTestConfig prepares the outbox output path, points the Database
// section at local MySQL (the MYSQL_DSN env var), then seedDB directly
// INSERTs into the model_services / endpoints / api_keys tables.
//
// If MYSQL_DSN isn't set, t.Skip the whole e2e test suite.
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
		UsageEvents: config.UsageEventsConfig{
			Driver: "file",
			File:   config.FileOutboxSection{Path: filepath.Join(dir, "usage.log")},
		},
		Request: config.RequestConfig{
			BodyLimitBytes: 10 << 20,
			Timeout:        5 * time.Second,
		},
		DataKey: devDataKey,
	}
	cfg.ApplyDefaults()
	return cfg
}

// seedDB connects to local MySQL, Migrates + TRUNCATEs, and writes a test
// ModelService + Endpoint + APIKey.
//
// **endpoint's auth/routing go through NamedExec** so that AuthConfig.Value()
// encrypts and RoutingConfig.Value() serializes to JSON—a raw SQL string
// wouldn't get that magic.
//
// **api_key goes through a hash**: SHA-256(plaintext) lands in the
// api_key_hash column, so the gateway's Resolve can look it up after hashing
// the incoming value.
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
	// MySQL refuses to TRUNCATE a parent table while an FK references it
	// (pricing_versions → model_services); disable FK checks and sweep through all at once
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

	// accounts("default") must exist first (FK anchor; schema.sql's seed does
	// INSERT IGNORE but TRUNCATE just wiped it out)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO accounts (pin, name) VALUES (?, ?)`,
		"default", "Default Account",
	); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	// model_services is a global catalog (no account_id / group_name / spec_detail)
	res, err := db.ExecContext(ctx,
		`INSERT INTO model_services (service_id, model) VALUES (?, ?)`,
		"openai/gpt-4o", "gpt-4o",
	)
	if err != nil {
		t.Fatalf("seed model_service: %v", err)
	}
	msID, _ := res.LastInsertId()

	// the account must be subscribed to the model to use it (otherwise M5 → 403)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO account_model_subscriptions (account_id, model_service_id, enabled) VALUES (?, ?, 1)`,
		"default", msID,
	); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}

	// M5 requires an active price or it returns 503; the e2e test must seed a price
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
		Name:     "openai_main",
		Vendor:   "openai",
		Protocol: "openai",
		Model:    "gpt-4o",
		Group:    "default",
		Weight:   100,
		Enabled:  true,
		Auth:     auth,
		Routing:  repo.RoutingConfig{URL: upstreamURL},
	}
	if _, err := db.NamedExecContext(ctx,
		`INSERT INTO endpoints
		 (name, vendor, protocol, model, group_name, weight, enabled, auth, routing, quota, capabilities, extra)
		 VALUES (:name, :vendor, :protocol, :model, :group_name, :weight, :enabled, :auth, :routing, :quota, :capabilities, :extra)`,
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
