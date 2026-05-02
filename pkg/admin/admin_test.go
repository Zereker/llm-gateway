package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	"github.com/zereker-labs/ai-gateway/pkg/infra"
)

const testToken = "test-admin-token"

// newTestEngine 起 gorm.DB（连本地 MySQL）+ Migrate + TRUNCATE + 完整 admin engine。
//
// MYSQL_DSN 没设就 t.Skip。本地：`docker compose up -d mysql` 后 export env。
func newTestEngine(t *testing.T) *gin.Engine {
	t.Helper()

	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("MYSQL_DSN not set; skipping admin integration test")
	}

	// schema 由 infra.Migrate 跑 raw SQL（gorm 不开 AutoMigrate）
	sqldb, err := infra.Open(infra.DBConfig{Driver: infra.DriverMySQL, DSN: dsn})
	if err != nil {
		t.Fatalf("infra.Open: %v", err)
	}
	if err := infra.Migrate(context.Background(), sqldb); err != nil {
		_ = sqldb.Close()
		t.Fatalf("infra.Migrate: %v", err)
	}
	for _, table := range []string{"api_keys", "endpoints", "model_services"} {
		if _, err := sqldb.Exec("TRUNCATE TABLE " + table); err != nil {
			_ = sqldb.Close()
			t.Fatalf("TRUNCATE %s: %v", table, err)
		}
	}
	t.Cleanup(func() { _ = sqldb.Close() })

	// gorm 复用同一份 *sql.DB（共享连接池）
	gdb, err := gorm.Open(mysql.New(mysql.Config{Conn: sqldb.DB}), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}

	return NewEngine(Deps{
		Token:             testToken,
		ModelServiceStore: NewModelServiceStore(gdb),
		EndpointStore:     NewEndpointStore(gdb),
		APIKeyStore:       NewAPIKeyStore(gdb),
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
	_ = newTestEngine(t) // ensure stack online + skip behavior aligned

	dsn := os.Getenv("MYSQL_DSN")
	sqldb, _ := infra.Open(infra.DBConfig{Driver: infra.DriverMySQL, DSN: dsn})
	t.Cleanup(func() { _ = sqldb.Close() })
	gdb, _ := gorm.Open(mysql.New(mysql.Config{Conn: sqldb.DB}), &gorm.Config{})

	engine := NewEngine(Deps{
		Token:             "",
		ModelServiceStore: NewModelServiceStore(gdb),
		EndpointStore:     NewEndpointStore(gdb),
		APIKeyStore:       NewAPIKeyStore(gdb),
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

// === apikey CRUD ===

func TestAPIKey_CreateReturnsPlaintextOnce(t *testing.T) {
	engine := newTestEngine(t)

	w := do(t, engine, "POST", "/admin/v1/apikeys", apiKeyCreateRequest{
		UserID:       "alice",
		Group:        "default",
		ExternalUser: false,
	})
	if w.Code != 201 {
		t.Fatalf("create status = %d body = %s", w.Code, w.Body.String())
	}
	var resp apiKeyCreateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.APIKey == "" {
		t.Error("Create response should include plaintext api_key")
	}
	if !bytes.HasPrefix([]byte(resp.APIKey), []byte("sk-")) {
		t.Errorf("api_key should be sk-prefixed, got %q", resp.APIKey)
	}
	if resp.APIKeyID == "" {
		t.Error("api_key_id should be backfilled")
	}
	if !resp.Enabled {
		t.Error("new key should default enabled")
	}
	if resp.TenantID != "default" {
		t.Errorf("tenant_id = %q, want default", resp.TenantID)
	}

	// 后续 GET 不返明文
	w = do(t, engine, "GET", "/admin/v1/apikeys/"+resp.APIKeyID, nil)
	if w.Code != 200 {
		t.Fatalf("get status = %d", w.Code)
	}
	if bytes.Contains(w.Body.Bytes(), []byte(`"api_key"`)) {
		t.Errorf("GET response leaked api_key plaintext: %s", w.Body.String())
	}
}

func TestAPIKey_ListByUser(t *testing.T) {
	engine := newTestEngine(t)
	for _, u := range []string{"alice", "alice", "bob"} {
		w := do(t, engine, "POST", "/admin/v1/apikeys", apiKeyCreateRequest{UserID: u})
		if w.Code != 201 {
			t.Fatalf("create for %s: %d", u, w.Code)
		}
	}

	// alice 有 2 个 key
	w := do(t, engine, "GET", "/admin/v1/apikeys?user_id=alice", nil)
	var resp struct {
		Items []apiKeyDTO `json:"items"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Items) != 2 {
		t.Errorf("alice should have 2 keys, got %d", len(resp.Items))
	}
}

func TestAPIKey_ToggleEnabled(t *testing.T) {
	engine := newTestEngine(t)
	w := do(t, engine, "POST", "/admin/v1/apikeys", apiKeyCreateRequest{UserID: "alice"})
	var created apiKeyCreateResponse
	_ = json.Unmarshal(w.Body.Bytes(), &created)

	// disable
	disabled := false
	w = do(t, engine, "PUT", "/admin/v1/apikeys/"+created.APIKeyID, apiKeyUpdateRequest{
		Enabled: &disabled,
	})
	if w.Code != 200 {
		t.Fatalf("update status = %d body = %s", w.Code, w.Body.String())
	}
	var got apiKeyDTO
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Enabled {
		t.Error("key should be disabled after PUT")
	}
}

func TestAPIKey_SetExpiresAt(t *testing.T) {
	engine := newTestEngine(t)
	w := do(t, engine, "POST", "/admin/v1/apikeys", apiKeyCreateRequest{UserID: "alice"})
	var created apiKeyCreateResponse
	_ = json.Unmarshal(w.Body.Bytes(), &created)

	expires := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	w = do(t, engine, "PUT", "/admin/v1/apikeys/"+created.APIKeyID, apiKeyUpdateRequest{
		ExpiresAt: &expires,
	})
	if w.Code != 200 {
		t.Fatalf("update status = %d", w.Code)
	}
	var got apiKeyDTO
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.ExpiresAt == nil {
		t.Fatal("expires_at not set")
	}
	if !got.ExpiresAt.Equal(expires) {
		t.Errorf("expires_at = %v, want %v", got.ExpiresAt, expires)
	}
}

func TestAPIKey_Delete(t *testing.T) {
	engine := newTestEngine(t)
	w := do(t, engine, "POST", "/admin/v1/apikeys", apiKeyCreateRequest{UserID: "alice"})
	var created apiKeyCreateResponse
	_ = json.Unmarshal(w.Body.Bytes(), &created)

	w = do(t, engine, "DELETE", "/admin/v1/apikeys/"+created.APIKeyID, nil)
	if w.Code != 204 {
		t.Errorf("delete status = %d, want 204", w.Code)
	}
	w = do(t, engine, "GET", "/admin/v1/apikeys/"+created.APIKeyID, nil)
	if w.Code != 404 {
		t.Errorf("after delete, get status = %d, want 404", w.Code)
	}
}
