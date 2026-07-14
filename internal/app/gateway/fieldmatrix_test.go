package gateway

// Field-matrix e2e tests: full-parameter requests through the real middleware
// chain against an echoing upstream, asserting the gateway forwards every
// top-level field untouched (or, for the translated path, the documented
// subset). The fixtures under internal/cassette/testdata/fieldmatrix/ were converged against a
// live OpenAI-compatible vendor: every field in them is accepted by at least
// one real upstream, so a field the gateway drops or mangles is a gateway
// defect, not a vendor quirk.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/zereker/llm-gateway/internal/cassette"
	"github.com/zereker/llm-gateway/internal/infra"
	"github.com/zereker/llm-gateway/internal/repo"
)

// loadFixture reads an internal/cassette/testdata/fieldmatrix JSON body and its parsed map form.
func loadFixture(t *testing.T, name string) ([]byte, map[string]any) {
	t.Helper()

	body, err := os.ReadFile(cassette.TestdataPath("fieldmatrix", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}

	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}

	return body, m
}

// upstreamFixture reads a captured real-vendor response body from
// internal/cassette/testdata/fieldmatrix/upstream (see the README there for provenance).
func upstreamFixture(t *testing.T, name string) []byte {
	t.Helper()

	body, err := os.ReadFile(cassette.TestdataPath("fieldmatrix", "upstream", name))
	if err != nil {
		t.Fatalf("read upstream fixture %s: %v", name, err)
	}

	return body
}

// captureUpstream returns an httptest server that records the last request
// body and replies with a fixed payload.
func captureUpstream(t *testing.T, reply string) (*httptest.Server, *[]byte) {
	t.Helper()

	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf [1 << 20]byte
		n, _ := r.Body.Read(buf[:])
		captured = append([]byte(nil), buf[:n]...)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))

	return srv, &captured
}

// assertFieldsForwarded checks that every field of want exists in got with a
// deep-equal value (JSON round-trip semantics).
func assertFieldsForwarded(t *testing.T, got []byte, want map[string]any) {
	t.Helper()

	var gotMap map[string]any
	if err := json.Unmarshal(got, &gotMap); err != nil {
		t.Fatalf("upstream captured body not JSON: %v; body=%s", err, got)
	}

	for k, v := range want {
		gv, ok := gotMap[k]
		if !ok {
			t.Errorf("field %q dropped before upstream", k)
			continue
		}

		if !reflect.DeepEqual(gv, v) {
			t.Errorf("field %q mangled: got %v, want %v", k, gv, v)
		}
	}
}

// TestE2E_FieldMatrix_ChatPassthrough: OpenAI chat client → openai-protocol
// upstream is an identity path — all 23 fields must arrive verbatim.
func TestE2E_FieldMatrix_ChatPassthrough(t *testing.T) {
	upstream, captured := captureUpstream(t, string(upstreamFixture(t, "chat-openai-compat.json")))
	defer upstream.Close()

	cfg := writeTestConfig(t, upstream.URL)
	engine, srv, err := buildEngine(cfg)
	if err != nil {
		t.Fatalf("buildEngine: %v", err)
	}
	defer srv.Close()

	body, want := loadFixture(t, "chat-full.json")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test-alice")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	assertFieldsForwarded(t, *captured, want)
}

// TestE2E_FieldMatrix_ResponsesIdentity: Responses client → responses-protocol
// upstream is an identity path — all fields arrive verbatim, the response body
// passes through untouched, and billing must read the Responses usage shape
// (input_tokens/output_tokens — regression: the OpenAI chat extractor billed
// zero in/out here).
func TestE2E_FieldMatrix_ResponsesIdentity(t *testing.T) {
	reply := upstreamFixture(t, "responses-native.json")
	upstream, captured := captureUpstream(t, string(reply))
	defer upstream.Close()

	// billing must match the real fixture's usage exactly
	var replyBody struct {
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(reply, &replyBody); err != nil || replyBody.Usage.InputTokens == 0 {
		t.Fatalf("fixture usage unreadable: %v", err)
	}

	cfg := writeTestConfig(t, upstream.URL)
	// flip the seeded endpoint to a native Responses upstream
	db, err := infra.Open(infra.DBConfig{Driver: infra.DriverMySQL, DSN: cfg.Database.DSN})
	if err != nil {
		t.Fatalf("infra.Open: %v", err)
	}
	if _, err := db.ExecContext(context.Background(),
		`UPDATE endpoints SET protocol = 'responses' WHERE name = 'openai_main'`); err != nil {
		t.Fatalf("flip endpoint protocol: %v", err)
	}
	_ = db.Close()

	engine, srv, err := buildEngine(cfg)
	if err != nil {
		t.Fatalf("buildEngine: %v", err)
	}
	defer srv.Close()

	body, want := loadFixture(t, "responses-full.json")
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test-alice")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	assertFieldsForwarded(t, *captured, want)

	// response passthrough: the native body reaches the client untouched
	var clientResp struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &clientResp); err != nil || clientResp.Status != "completed" {
		t.Errorf("response lost status field (err=%v): %s", err, w.Body.String())
	}

	// billing: the Responses usage field names must be extracted
	usageLog, _ := os.ReadFile(cfg.UsageEvents.File.Path)

	wantIn := fmt.Sprintf(`"input":%d`, replyBody.Usage.InputTokens)
	wantOut := fmt.Sprintf(`"output":%d`, replyBody.Usage.OutputTokens)
	if !strings.Contains(string(usageLog), wantIn) || !strings.Contains(string(usageLog), wantOut) {
		t.Errorf("usage log missing %s/%s from Responses usage; got: %s", wantIn, wantOut, usageLog)
	}
}

// TestE2E_FieldMatrix_ResponsesTranslated: Responses client → openai-protocol
// upstream goes through the responses_openai translator, which forwards only a
// documented subset (model / instructions+input text → messages /
// max_output_tokens → max_tokens / temperature / top_p; benign hints like
// reasoning / text / store are dropped). This test pins the supported subset
// and the Responses-shaped reply (status/created_at).
func TestE2E_FieldMatrix_ResponsesTranslated(t *testing.T) {
	reply := upstreamFixture(t, "chat-openai-compat.json")
	upstream, captured := captureUpstream(t, string(reply))
	defer upstream.Close()

	var replyBody struct {
		Created int64 `json:"created"`
	}
	if err := json.Unmarshal(reply, &replyBody); err != nil || replyBody.Created == 0 {
		t.Fatalf("fixture created unreadable: %v", err)
	}

	cfg := writeTestConfig(t, upstream.URL)
	engine, srv, err := buildEngine(cfg)
	if err != nil {
		t.Fatalf("buildEngine: %v", err)
	}
	defer srv.Close()

	body, _ := loadFixture(t, "responses-text.json")
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test-alice")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var chat struct {
		Model       string `json:"model"`
		MaxTokens   int    `json:"max_tokens"`
		Temperature float64
		TopP        float64 `json:"top_p"`
		Messages    []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(*captured, &chat); err != nil {
		t.Fatalf("captured chat body not JSON: %v; body=%s", err, *captured)
	}

	if chat.Model != "gpt-4o" || chat.MaxTokens != 2048 || chat.TopP != 0.9 {
		t.Errorf("mapped params wrong: %+v", chat)
	}

	if len(chat.Messages) != 2 || chat.Messages[0].Role != "system" || chat.Messages[1].Role != "user" {
		t.Fatalf("messages mapping wrong: %+v", chat.Messages)
	}

	if !strings.Contains(chat.Messages[1].Content, "图片主要讲了什么") {
		t.Errorf("input_text lost: %q", chat.Messages[1].Content)
	}

	// reply must be Responses-shaped with the completion contract fields
	wantCreated := fmt.Sprintf(`"created_at":%d`, replyBody.Created)
	for _, wantFrag := range []string{`"object":"response"`, `"status":"completed"`, wantCreated} {
		if !strings.Contains(w.Body.String(), wantFrag) {
			t.Errorf("translated reply missing %s: %s", wantFrag, w.Body.String())
		}
	}
}

// TestE2E_FieldMatrix_AnthropicStreamBilling replays a real anthropic-compatible
// SSE capture through the /v1/messages identity path. The capture carries the
// vendor variant that regressed billing once: message_start reports
// input_tokens 0 and the full usage arrives in message_delta.
func TestE2E_FieldMatrix_AnthropicStreamBilling(t *testing.T) {
	sse := upstreamFixture(t, "messages-anthropic-compat-stream.sse")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(sse)
	}))
	defer upstream.Close()

	// the billing expectation comes from the capture's message_delta usage —
	// the LAST usage in the stream (message_start's leading zeros come first)
	all := regexp.MustCompile(`"usage":\{"input_tokens":(\d+),"output_tokens":(\d+)\}`).FindAllSubmatch(sse, -1)
	if len(all) == 0 {
		t.Fatal("fixture lost its message_delta usage; re-capture it")
	}

	m := all[len(all)-1]

	cfg := writeTestConfig(t, upstream.URL)
	// flip the seeded endpoint to an anthropic upstream (x-api-key auth)
	db, err := infra.Open(infra.DBConfig{Driver: infra.DriverMySQL, DSN: cfg.Database.DSN})
	if err != nil {
		t.Fatalf("infra.Open: %v", err)
	}

	auth, err := repo.EncodePayload(repo.AuthTypeXAPIKey, repo.XAPIKeyAuth{APIKey: "sk-upstream-key"})
	if err != nil {
		t.Fatalf("encode x-api-key: %v", err)
	}

	if _, err := db.ExecContext(context.Background(),
		`UPDATE endpoints SET vendor = 'anthropic', protocol = 'anthropic', auth = ? WHERE name = 'openai_main'`,
		auth); err != nil {
		t.Fatalf("flip endpoint to anthropic: %v", err)
	}
	_ = db.Close()

	engine, srv, err := buildEngine(cfg)
	if err != nil {
		t.Fatalf("buildEngine: %v", err)
	}
	defer srv.Close()

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"gpt-4o","max_tokens":512,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test-alice")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	if !strings.Contains(w.Body.String(), "message_stop") {
		t.Errorf("client stream missing message_stop: %s", w.Body.String()[:200])
	}

	usageLog, _ := os.ReadFile(cfg.UsageEvents.File.Path)

	wantIn, wantOut := fmt.Sprintf(`"input":%s`, m[1]), fmt.Sprintf(`"output":%s`, m[2])
	if !strings.Contains(string(usageLog), wantIn) || !strings.Contains(string(usageLog), wantOut) {
		t.Errorf("stream billing missing %s/%s; got: %s", wantIn, wantOut, usageLog)
	}
}

// TestE2E_FieldMatrix_ResponsesTranslatedFailFast: fields that cannot survive
// the responses→chat translation (input_image parts, tool definitions) must be
// rejected with 400 invalid — never silently dropped and billed.
func TestE2E_FieldMatrix_ResponsesTranslatedFailFast(t *testing.T) {
	upstream, captured := captureUpstream(t, `{}`)
	defer upstream.Close()

	cfg := writeTestConfig(t, upstream.URL)
	engine, srv, err := buildEngine(cfg)
	if err != nil {
		t.Fatalf("buildEngine: %v", err)
	}
	defer srv.Close()

	// responses-full.json carries both input_image and tools
	body, _ := loadFixture(t, "responses-full.json")
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test-alice")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}

	if !strings.Contains(w.Body.String(), `"class":"invalid"`) {
		t.Errorf("error not classified invalid: %s", w.Body.String())
	}

	if len(*captured) != 0 {
		t.Errorf("request must fail before reaching the upstream, but upstream saw: %s", *captured)
	}
}
