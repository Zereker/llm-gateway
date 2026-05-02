package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/infra"
	"github.com/zereker-labs/ai-gateway/pkg/repo"
)

const testToken = "test-admin-token"

// newTestEngine 起一份独立 sqlite + 完整 admin engine。
//
// 直接用 NewEngine + 真实 SQL repo（而不是 stub），因为 admin 的整个价值就是
// "从 HTTP 把请求落到 DB"——stub repo 等于绕开测试目标。
func newTestEngine(t *testing.T) *gin.Engine {
	t.Helper()
	dir := t.TempDir()

	db, err := infra.Open(infra.DBConfig{Driver: infra.DriverSQLite, DSN: filepath.Join(dir, "admin.db")})
	if err != nil {
		t.Fatalf("infra.Open: %v", err)
	}
	if err := infra.Migrate(context.Background(), db); err != nil {
		_ = db.Close()
		t.Fatalf("infra.Migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return NewEngine(Deps{
		Token:            testToken,
		ModelServiceRepo: repo.NewSQLModelServiceRepo(db),
		EndpointRepo:     repo.NewSQLEndpointRepo(db),
	})
}

func do(t *testing.T, engine *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(adminTokenHeader, testToken)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

// === auth ===

func TestAuth_RejectsMissingToken(t *testing.T) {
	engine := newTestEngine(t)
	req := httptest.NewRequest("GET", "/admin/v1/modelservices", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestAuth_RejectsWrongToken(t *testing.T) {
	engine := newTestEngine(t)
	req := httptest.NewRequest("GET", "/admin/v1/modelservices", nil)
	req.Header.Set(adminTokenHeader, "nope")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestAuth_OpsBypassToken(t *testing.T) {
	engine := newTestEngine(t)
	for _, p := range []string{"/healthz", "/readyz"} {
		req := httptest.NewRequest("GET", p, nil)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Errorf("%s: status = %d", p, w.Code)
		}
	}
}

func TestAuth_EmptyConfiguredTokenRefusesAll(t *testing.T) {
	// 即便 caller 也送了 token，服务侧 token 没配就拒（防误配上线）。
	dir := t.TempDir()
	db, _ := infra.Open(infra.DBConfig{Driver: infra.DriverSQLite, DSN: filepath.Join(dir, "x.db")})
	_ = infra.Migrate(context.Background(), db)
	t.Cleanup(func() { _ = db.Close() })
	engine := NewEngine(Deps{
		Token:            "",
		ModelServiceRepo: repo.NewSQLModelServiceRepo(db),
		EndpointRepo:     repo.NewSQLEndpointRepo(db),
	})

	req := httptest.NewRequest("GET", "/admin/v1/modelservices", nil)
	req.Header.Set(adminTokenHeader, "anything")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != 500 {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// === modelservice CRUD ===

func TestModelService_FullCRUD(t *testing.T) {
	engine := newTestEngine(t)

	// CREATE
	w := do(t, engine, "POST", "/admin/v1/modelservices", modelServiceDTO{
		ServiceID: "openai/gpt-4o", Model: "gpt-4o", Group: "default", Tpm: 100000, Rpm: 600,
	})
	if w.Code != 201 {
		t.Fatalf("create status = %d body = %s", w.Code, w.Body.String())
	}
	var created modelServiceDTO
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if created.ID == 0 {
		t.Errorf("created.ID should be backfilled")
	}

	// LIST
	w = do(t, engine, "GET", "/admin/v1/modelservices", nil)
	if w.Code != 200 {
		t.Fatalf("list status = %d", w.Code)
	}
	var listResp struct {
		Items []modelServiceDTO `json:"items"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &listResp)
	if len(listResp.Items) != 1 || listResp.Items[0].Model != "gpt-4o" {
		t.Errorf("list = %+v", listResp.Items)
	}

	// GET by model
	w = do(t, engine, "GET", "/admin/v1/modelservices/gpt-4o", nil)
	if w.Code != 200 {
		t.Fatalf("get status = %d", w.Code)
	}
	var got modelServiceDTO
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Tpm != 100000 {
		t.Errorf("got.Tpm = %d", got.Tpm)
	}

	// UPDATE
	w = do(t, engine, "PUT", "/admin/v1/modelservices/gpt-4o", modelServiceDTO{
		ServiceID: "openai/gpt-4o", Model: "gpt-4o", Group: "default", Tpm: 999, Rpm: 50,
	})
	if w.Code != 200 {
		t.Fatalf("update status = %d body = %s", w.Code, w.Body.String())
	}
	w = do(t, engine, "GET", "/admin/v1/modelservices/gpt-4o", nil)
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Tpm != 999 {
		t.Errorf("after update, Tpm = %d", got.Tpm)
	}

	// DELETE
	w = do(t, engine, "DELETE", "/admin/v1/modelservices/gpt-4o", nil)
	if w.Code != 204 {
		t.Errorf("delete status = %d", w.Code)
	}
	w = do(t, engine, "GET", "/admin/v1/modelservices/gpt-4o", nil)
	if w.Code != 404 {
		t.Errorf("after delete, get status = %d, want 404", w.Code)
	}
}

func TestModelService_GetMissing(t *testing.T) {
	engine := newTestEngine(t)
	w := do(t, engine, "GET", "/admin/v1/modelservices/nope", nil)
	if w.Code != 404 {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestModelService_CreateBadJSON(t *testing.T) {
	engine := newTestEngine(t)
	req := httptest.NewRequest("POST", "/admin/v1/modelservices", bytes.NewReader([]byte("{not json}")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(adminTokenHeader, testToken)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// === endpoint CRUD ===

func TestEndpoint_FullCRUD(t *testing.T) {
	engine := newTestEngine(t)

	// CREATE
	w := do(t, engine, "POST", "/admin/v1/endpoints", endpointDTO{
		ID: "openai_main", Vendor: "openai", URL: "https://api.openai.com",
		APIKey: "sk-real-key", Group: "default", Model: "gpt-4o", Weight: 100,
	})
	if w.Code != 201 {
		t.Fatalf("create status = %d body = %s", w.Code, w.Body.String())
	}
	var created endpointDTO
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if created.APIKey != "sk-real-key" {
		t.Errorf("APIKey on POST response was masked: %q (DTO should expose plaintext)", created.APIKey)
	}

	// GET — 也要明文返回
	w = do(t, engine, "GET", "/admin/v1/endpoints/openai_main", nil)
	if w.Code != 200 {
		t.Fatalf("get status = %d", w.Code)
	}
	var got endpointDTO
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.APIKey != "sk-real-key" {
		t.Errorf("APIKey on GET response was masked: %q", got.APIKey)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"api_key"`)) {
		t.Errorf("response should use snake_case api_key: %s", w.Body.String())
	}

	// UPDATE
	w = do(t, engine, "PUT", "/admin/v1/endpoints/openai_main", endpointDTO{
		ID: "openai_main", Vendor: "openai", URL: "https://new.example.com",
		APIKey: "sk-rotated", Group: "default", Model: "gpt-4o", Weight: 200,
	})
	if w.Code != 200 {
		t.Fatalf("update status = %d body = %s", w.Code, w.Body.String())
	}
	w = do(t, engine, "GET", "/admin/v1/endpoints/openai_main", nil)
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.URL != "https://new.example.com" || got.APIKey != "sk-rotated" || got.Weight != 200 {
		t.Errorf("after update, got %+v", got)
	}

	// LIST
	w = do(t, engine, "GET", "/admin/v1/endpoints", nil)
	if w.Code != 200 {
		t.Fatalf("list status = %d", w.Code)
	}

	// DELETE
	w = do(t, engine, "DELETE", "/admin/v1/endpoints/openai_main", nil)
	if w.Code != 204 {
		t.Errorf("delete status = %d", w.Code)
	}
	w = do(t, engine, "GET", "/admin/v1/endpoints/openai_main", nil)
	if w.Code != 404 {
		t.Errorf("after delete, get status = %d, want 404", w.Code)
	}
}

func TestEndpoint_DuplicateID(t *testing.T) {
	engine := newTestEngine(t)
	body := endpointDTO{ID: "x", Vendor: "v", URL: "u", Model: "m"}
	w := do(t, engine, "POST", "/admin/v1/endpoints", body)
	if w.Code != 201 {
		t.Fatalf("first create: %d", w.Code)
	}
	w = do(t, engine, "POST", "/admin/v1/endpoints", body)
	if w.Code != 400 {
		t.Errorf("duplicate create status = %d, want 400", w.Code)
	}
}

func TestEndpoint_UpdateMissing(t *testing.T) {
	engine := newTestEngine(t)
	w := do(t, engine, "PUT", "/admin/v1/endpoints/nope", endpointDTO{
		ID: "nope", Vendor: "v", URL: "u", Model: "m",
	})
	if w.Code != 404 {
		t.Errorf("status = %d, want 404", w.Code)
	}
}
