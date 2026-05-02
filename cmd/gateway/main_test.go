package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// e2e: 用 httptest 模拟 OpenAI 上游，把 gateway 的全套 middleware 串起来跑一遍。
func TestE2E_OpenAIChatCompletions(t *testing.T) {
	// 1. 假上游：返回固定 OpenAI-shaped JSON
	var capturedAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer upstream.Close()

	// 2. 写一份测试 config 到临时目录
	dir := writeTestConfig(t, upstream.URL)

	// 3. 构造 engine
	engine, cleanup, err := buildEngine(options{
		ConfigDir: dir,
		UsageLog:  filepath.Join(dir, "usage.log"),
		BodyLimit: 10 << 20,
		Timeout:   5 * time.Second,
	})
	if err != nil {
		t.Fatalf("buildEngine: %v", err)
	}
	defer cleanup()

	// 4. 发请求
	body := `{"model":"gpt-4o","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test-alice")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	// 5. 断言响应
	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"content":"hi"`) {
		t.Errorf("body missing content: %s", w.Body.String())
	}

	// 6. 断言 Authorization 已替换为 endpoint 的 key（不是客户端的）
	if capturedAuth != "Bearer sk-upstream-key" {
		t.Errorf("upstream Authorization = %q, want Bearer sk-upstream-key", capturedAuth)
	}

	// 7. 断言 usage 已落 outbox
	usageLog, _ := os.ReadFile(filepath.Join(dir, "usage.log"))
	if !strings.Contains(string(usageLog), `"Total":15`) {
		t.Errorf("usage log missing Total:15; got: %s", usageLog)
	}
}

func TestE2E_RejectsMissingAuth(t *testing.T) {
	dir := writeTestConfig(t, "http://example.invalid")
	engine, cleanup, err := buildEngine(options{
		ConfigDir: dir, UsageLog: filepath.Join(dir, "usage.log"),
		BodyLimit: 10 << 20, Timeout: 5 * time.Second,
	})
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
	dir := writeTestConfig(t, "http://example.invalid")
	engine, cleanup, err := buildEngine(options{
		ConfigDir: dir, UsageLog: filepath.Join(dir, "usage.log"),
		BodyLimit: 10 << 20, Timeout: 5 * time.Second,
	})
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
	dir := writeTestConfig(t, "http://example.invalid")
	engine, cleanup, err := buildEngine(options{
		ConfigDir: dir, UsageLog: filepath.Join(dir, "usage.log"),
		BodyLimit: 10 << 20, Timeout: 5 * time.Second,
	})
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

// writeTestConfig builds the standard config layout in a t.TempDir() pointing
// the lone openai endpoint at upstreamURL.
//
// 注意：Endpoint.APIKey 是 domain.Secret，json.Marshal 它会屏蔽成 "***"；
// 配置文件本应由人手写真实 key，所以 endpoint 走字面量 JSON。
func writeTestConfig(t *testing.T, upstreamURL string) string {
	t.Helper()
	dir := t.TempDir()

	apikeys := map[string]domain.UserIdentity{
		"sk-test-alice": {UserID: "alice", APIKeyID: "ak_alice", Group: "default"},
	}
	mustWriteJSON(t, filepath.Join(dir, "apikeys.json"), apikeys)

	mustWriteJSON(t, filepath.Join(dir, "kv", "modelservice", "svc_gpt4o.json"), domain.ModelServiceSnapshot{
		ID: 1, ServiceID: "openai/gpt-4o", Model: "gpt-4o", Group: "default",
	})

	endpointJSON := []byte(`{
  "ID": "openai_main",
  "Vendor": "openai",
  "URL": ` + jsonString(upstreamURL) + `,
  "APIKey": "sk-upstream-key",
  "Group": "default",
  "Model": "gpt-4o"
}`)
	mustWriteFile(t, filepath.Join(dir, "kv", "endpoint", "openai_main.json"), endpointJSON)

	return dir
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func mustWriteJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, path, data)
}

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
