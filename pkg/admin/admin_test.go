package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	"github.com/zereker-labs/ai-gateway/pkg/infra"
	"github.com/zereker-labs/ai-gateway/pkg/repo"
)

const testToken = "test-admin-token"

// devDataKey 是 admin tests 用的 AES KEK；任何写 endpoints.auth 的测试都要靠这个。
const devDataKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func init() {
	if err := repo.SetDataKey(devDataKey); err != nil {
		panic("admin tests: SetDataKey: " + err.Error())
	}
}

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
	// FK 引用时 MySQL 拒 TRUNCATE 父表（pricing_versions → model_services）；
	// 关掉 FOREIGN_KEY_CHECKS 一把扫
	if _, err := sqldb.Exec(`SET FOREIGN_KEY_CHECKS = 0`); err != nil {
		_ = sqldb.Close()
		t.Fatalf("disable FK checks: %v", err)
	}
	for _, table := range []string{
		"pricing_versions",
		"tenant_model_subscriptions",
		"api_keys",
		"endpoints",
		"model_services",
		"tenants",
		"quota_policies",
	} {
		if _, err := sqldb.Exec("TRUNCATE TABLE " + table); err != nil {
			_ = sqldb.Close()
			t.Fatalf("TRUNCATE %s: %v", table, err)
		}
	}
	if _, err := sqldb.Exec(`SET FOREIGN_KEY_CHECKS = 1`); err != nil {
		_ = sqldb.Close()
		t.Fatalf("re-enable FK checks: %v", err)
	}
	// seed default tenant（FK 锚点；其它表 FK → tenants(pin)）
	if _, err := sqldb.Exec(`INSERT INTO tenants (pin, name) VALUES ('default', 'Default Tenant')`); err != nil {
		_ = sqldb.Close()
		t.Fatalf("seed default tenant: %v", err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })

	// gorm 复用同一份 *sql.DB（共享连接池）
	gdb, err := gorm.Open(mysql.New(mysql.Config{Conn: sqldb.DB}), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}

	return NewEngine(Deps{
		Token:             testToken,
		TenantStore:       NewTenantStore(gdb),
		QuotaPolicyStore:  NewQuotaPolicyStore(gdb),
		ModelServiceStore: NewModelServiceStore(gdb),
		SubscriptionStore: NewSubscriptionStore(gdb),
		EndpointStore:     NewEndpointStore(gdb),
		APIKeyStore:       NewAPIKeyStore(gdb),
		PricingStore:      NewPricingStore(gdb),
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

// bearerAuthConfig 测试 helper：构造 BearerAuth 的 AuthConfig。
//
// 直接用 repo.EncodePayload 等于在 admin DTO 边界这样收到 client 的请求体：
//
//	{"type":"bearer","payload":{"api_key":"sk-..."}}
func bearerAuthConfig(t *testing.T, key string) repo.AuthConfig {
	t.Helper()
	a, err := repo.EncodePayload(repo.AuthTypeBearer, repo.BearerAuth{APIKey: key})
	if err != nil {
		t.Fatalf("EncodePayload: %v", err)
	}
	return a
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
		TenantStore:       NewTenantStore(gdb),
		QuotaPolicyStore:  NewQuotaPolicyStore(gdb),
		ModelServiceStore: NewModelServiceStore(gdb),
		SubscriptionStore: NewSubscriptionStore(gdb),
		EndpointStore:     NewEndpointStore(gdb),
		APIKeyStore:       NewAPIKeyStore(gdb),
		PricingStore:      NewPricingStore(gdb),
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
		ServiceID: "openai/gpt-4o", Model: "gpt-4o",
	})
	if w.Code != 201 {
		t.Fatalf("create status = %d body = %s", w.Code, w.Body.String())
	}
	var created modelServiceDTO
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if created.ID == 0 {
		t.Errorf("created.ID should be backfilled")
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Errorf("audit timestamps not backfilled: %+v", created)
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

	// UPDATE
	w = do(t, engine, "PUT", "/admin/v1/modelservices/gpt-4o", modelServiceDTO{
		ServiceID: "openai/gpt-4o-rotated", Model: "gpt-4o",
	})
	if w.Code != 200 {
		t.Fatalf("update status = %d body = %s", w.Code, w.Body.String())
	}
	w = do(t, engine, "GET", "/admin/v1/modelservices/gpt-4o", nil)
	var got modelServiceDTO
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.ServiceID != "openai/gpt-4o-rotated" {
		t.Errorf("after update, ServiceID = %q", got.ServiceID)
	}

	// DELETE (soft)
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
		Name:    "openai_main",
		Vendor:  "openai",
		Model:   "gpt-4o",
		Group:   "default",
		Weight:  100,
		Auth:    bearerAuthConfig(t, "sk-real-key"),
		Routing: repo.RoutingConfig{URL: "https://api.openai.com/v1/chat/completions"},
	})
	if w.Code != 201 {
		t.Fatalf("create status = %d body = %s", w.Code, w.Body.String())
	}
	var created endpointDTO
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if created.ID == 0 {
		t.Fatal("created.ID should be backfilled")
	}
	createdID := created.ID

	// GET — auth payload 应该被屏蔽
	w = do(t, engine, "GET", endpointPath(createdID), nil)
	if w.Code != 200 {
		t.Fatalf("get status = %d", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"payload":"***"`)) {
		t.Errorf("auth payload should be masked in GET: %s", w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("sk-real-key")) {
		t.Errorf("plaintext key leaked in GET: %s", w.Body.String())
	}
	// 但 routing.url 应可见
	if !bytes.Contains(w.Body.Bytes(), []byte("api.openai.com")) {
		t.Errorf("routing.url missing: %s", w.Body.String())
	}

	// UPDATE — rotate API key
	w = do(t, engine, "PUT", endpointPath(createdID), endpointDTO{
		ID:      createdID,
		Name:    "openai_main",
		Vendor:  "openai",
		Model:   "gpt-4o",
		Group:   "default",
		Weight:  200,
		Enabled: true,
		Auth:    bearerAuthConfig(t, "sk-rotated"),
		Routing: repo.RoutingConfig{URL: "https://new.example.com"},
	})
	if w.Code != 200 {
		t.Fatalf("update status = %d body = %s", w.Code, w.Body.String())
	}

	// GET 后验证 weight + routing 已更新
	w = do(t, engine, "GET", endpointPath(createdID), nil)
	var got endpointDTO
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Weight != 200 || got.Routing.URL != "https://new.example.com" {
		t.Errorf("after update, got %+v", got)
	}

	// LIST
	w = do(t, engine, "GET", "/admin/v1/endpoints", nil)
	if w.Code != 200 {
		t.Fatalf("list status = %d", w.Code)
	}

	// LIST by name
	w = do(t, engine, "GET", "/admin/v1/endpoints?name=openai_main", nil)
	if w.Code != 200 {
		t.Fatalf("list by name status = %d", w.Code)
	}

	// DELETE (soft)
	w = do(t, engine, "DELETE", endpointPath(createdID), nil)
	if w.Code != 204 {
		t.Errorf("delete status = %d", w.Code)
	}
	w = do(t, engine, "GET", endpointPath(createdID), nil)
	if w.Code != 404 {
		t.Errorf("after delete, get status = %d, want 404", w.Code)
	}
}

func TestEndpoint_DuplicateName(t *testing.T) {
	engine := newTestEngine(t)
	body := endpointDTO{
		Name: "x", Vendor: "openai", Model: "m",
		Auth:    bearerAuthConfig(t, "k"),
		Routing: repo.RoutingConfig{URL: "u"},
	}
	w := do(t, engine, "POST", "/admin/v1/endpoints", body)
	if w.Code != 201 {
		t.Fatalf("first create: %d body=%s", w.Code, w.Body.String())
	}
	w = do(t, engine, "POST", "/admin/v1/endpoints", body)
	if w.Code != 400 {
		t.Errorf("duplicate create status = %d, want 400", w.Code)
	}
}

func TestEndpoint_UpdateMissing(t *testing.T) {
	engine := newTestEngine(t)
	w := do(t, engine, "PUT", endpointPath(99999), endpointDTO{
		Name: "nope", Vendor: "openai", Model: "m",
		Auth:    bearerAuthConfig(t, "k"),
		Routing: repo.RoutingConfig{URL: "u"},
	})
	if w.Code != 404 {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestEndpoint_GetByIDInvalidParam(t *testing.T) {
	engine := newTestEngine(t)
	w := do(t, engine, "GET", "/admin/v1/endpoints/notanumber", nil)
	if w.Code != 400 {
		t.Errorf("status = %d, want 400 (invalid id)", w.Code)
	}
}

func endpointPath(id int64) string {
	return "/admin/v1/endpoints/" + strconv.FormatInt(id, 10)
}

// === apikey CRUD ===

func TestAPIKey_CreateReturnsPlaintextOnce(t *testing.T) {
	engine := newTestEngine(t)

	w := do(t, engine, "POST", "/admin/v1/apikeys", apiKeyCreateRequest{
		UserID:       "alice",
		Name:         "prod",
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
	if resp.APIKeyPrefix == "" {
		t.Error("api_key_prefix should be backfilled")
	}
	if !bytes.HasPrefix([]byte(resp.APIKey), []byte(resp.APIKeyPrefix)) {
		t.Errorf("plaintext should start with prefix: prefix=%q full=%q", resp.APIKeyPrefix, resp.APIKey)
	}
	if resp.APIKeyID == "" {
		t.Error("api_key_id should be backfilled")
	}
	if !resp.Enabled {
		t.Error("new key should default enabled")
	}
	if resp.Name != "prod" {
		t.Errorf("name = %q", resp.Name)
	}
	if resp.TenantID != "default" {
		t.Errorf("tenant_id = %q, want default", resp.TenantID)
	}

	// 后续 GET 不返明文 api_key（但会返 prefix）
	w = do(t, engine, "GET", "/admin/v1/apikeys/"+resp.APIKeyID, nil)
	if w.Code != 200 {
		t.Fatalf("get status = %d", w.Code)
	}
	if bytes.Contains(w.Body.Bytes(), []byte(`"api_key":"sk-`)) {
		t.Errorf("GET response leaked api_key plaintext: %s", w.Body.String())
	}
	// prefix 应该可见
	if !bytes.Contains(w.Body.Bytes(), []byte(`"api_key_prefix":"sk-`)) {
		t.Errorf("prefix missing in GET: %s", w.Body.String())
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

func TestAPIKey_Revoke(t *testing.T) {
	engine := newTestEngine(t)
	w := do(t, engine, "POST", "/admin/v1/apikeys", apiKeyCreateRequest{UserID: "alice"})
	var created apiKeyCreateResponse
	_ = json.Unmarshal(w.Body.Bytes(), &created)

	w = do(t, engine, "POST", "/admin/v1/apikeys/"+created.APIKeyID+"/revoke", nil)
	if w.Code != 200 {
		t.Fatalf("revoke status = %d body = %s", w.Code, w.Body.String())
	}
	var got apiKeyDTO
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.RevokedAt == nil {
		t.Error("revoked_at should be set")
	}
	if got.Enabled {
		t.Error("revoked key should be disabled")
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

// === pricing CRUD ===

func TestPricing_RotateAppendsHistory(t *testing.T) {
	engine := newTestEngine(t)

	// 先建一个 model_service（pricing 需要 FK）
	w := do(t, engine, "POST", "/admin/v1/modelservices", modelServiceDTO{
		ServiceID: "openai/gpt-4o", Model: "gpt-4o",
	})
	if w.Code != 201 {
		t.Fatalf("create ms: %d %s", w.Code, w.Body.String())
	}

	// 第一次发版（首次 publish）
	w = do(t, engine, "POST", "/admin/v1/modelservices/gpt-4o/prices", pricingRotateRequest{
		RuleClass: "standard",
		RuleJSON:  json.RawMessage(`{"unit":"tokens_per_1m","currency":"USD","rates":{"input":5.0,"output":15.0}}`),
		CreatedBy: "alice",
		Notes:     "initial price",
	})
	if w.Code != 201 {
		t.Fatalf("rotate v1: %d %s", w.Code, w.Body.String())
	}
	var v1 pricingVersionDTO
	_ = json.Unmarshal(w.Body.Bytes(), &v1)
	if v1.ID == 0 || v1.EffectiveTo != nil {
		t.Errorf("v1 should be active (effective_to nil); got %+v", v1)
	}

	// 第二次发版：应该接替 v1（v1 effective_to 被设为现在，v2 effective_to=NULL）
	time.Sleep(10 * time.Millisecond) // 让 effective_from 区分开
	w = do(t, engine, "POST", "/admin/v1/modelservices/gpt-4o/prices", pricingRotateRequest{
		RuleJSON:  json.RawMessage(`{"unit":"tokens_per_1m","currency":"USD","rates":{"input":3.0,"output":10.0}}`),
		CreatedBy: "alice",
		Notes:     "rate cut",
	})
	if w.Code != 201 {
		t.Fatalf("rotate v2: %d %s", w.Code, w.Body.String())
	}
	var v2 pricingVersionDTO
	_ = json.Unmarshal(w.Body.Bytes(), &v2)

	// active 现在应该是 v2
	w = do(t, engine, "GET", "/admin/v1/modelservices/gpt-4o/prices/active", nil)
	if w.Code != 200 {
		t.Fatalf("get active: %d", w.Code)
	}
	var active pricingVersionDTO
	_ = json.Unmarshal(w.Body.Bytes(), &active)
	if active.ID != v2.ID {
		t.Errorf("active ID = %d, want v2 ID %d", active.ID, v2.ID)
	}

	// 历史应有 2 条
	w = do(t, engine, "GET", "/admin/v1/modelservices/gpt-4o/prices", nil)
	if w.Code != 200 {
		t.Fatalf("list history: %d", w.Code)
	}
	var listResp struct {
		Items []pricingVersionDTO `json:"items"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &listResp)
	if len(listResp.Items) != 2 {
		t.Fatalf("history len = %d, want 2", len(listResp.Items))
	}
	// 倒序：最新在 [0]
	if listResp.Items[0].ID != v2.ID || listResp.Items[1].ID != v1.ID {
		t.Errorf("order wrong: got [%d, %d], want [%d, %d]",
			listResp.Items[0].ID, listResp.Items[1].ID, v2.ID, v1.ID)
	}
	// v1 should now be sealed (effective_to set to v2.effective_from)
	if listResp.Items[1].EffectiveTo == nil {
		t.Error("v1 should be sealed after v2 rotate")
	}
}

func TestPricing_ActiveMissingReturns404(t *testing.T) {
	engine := newTestEngine(t)

	// 建 ms 但不发价
	w := do(t, engine, "POST", "/admin/v1/modelservices", modelServiceDTO{
		ServiceID: "openai/gpt-4o", Model: "gpt-4o",
	})
	if w.Code != 201 {
		t.Fatalf("create ms: %d", w.Code)
	}

	w = do(t, engine, "GET", "/admin/v1/modelservices/gpt-4o/prices/active", nil)
	if w.Code != 404 {
		t.Errorf("status = %d, want 404 (no active price)", w.Code)
	}
}

func TestPricing_BadRequestEmptyRule(t *testing.T) {
	engine := newTestEngine(t)
	w := do(t, engine, "POST", "/admin/v1/modelservices", modelServiceDTO{
		ServiceID: "openai/gpt-4o", Model: "gpt-4o",
	})
	if w.Code != 201 {
		t.Fatalf("create ms: %d", w.Code)
	}
	// 没 rule_json
	w = do(t, engine, "POST", "/admin/v1/modelservices/gpt-4o/prices", pricingRotateRequest{
		RuleClass: "standard",
	})
	if w.Code != 400 {
		t.Errorf("status = %d, want 400 (empty rule_json)", w.Code)
	}
}

// === gateway-end-to-end: admin POST + gateway resolve ===

// TestEndToEnd_AdminCreatesKey_GatewayResolves 创建 api key → SQLAPIKeyProvider hash 后能查到。
//
// 验证 api_key SHA-256 hash 链路对齐：admin Create 用 HashAPIKey 落 DB，
// gateway Resolve 用同一函数 hash 入参。错位会让所有请求 401。
func TestEndToEnd_AdminCreatesKey_GatewayResolves(t *testing.T) {
	engine := newTestEngine(t)

	// 用 admin 创建一个 key，拿到明文
	w := do(t, engine, "POST", "/admin/v1/apikeys", apiKeyCreateRequest{
		UserID: "alice",
		Name:   "e2e",
	})
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created apiKeyCreateResponse
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	plaintext := created.APIKey

	// 直接连同一个 DB 起 SQLAPIKeyProvider
	dsn := os.Getenv("MYSQL_DSN")
	sqldb, _ := infra.Open(infra.DBConfig{Driver: infra.DriverMySQL, DSN: dsn})
	t.Cleanup(func() { _ = sqldb.Close() })

	provider := repo.NewSQLAPIKeyProvider(sqldb)
	id, err := provider.Resolve(context.Background(), &repo.Credentials{APIKey: plaintext})
	if err != nil {
		t.Fatalf("Resolve plaintext: %v", err)
	}
	if id.UserID != "alice" {
		t.Errorf("UserID = %q, want alice", id.UserID)
	}

	// 错的 key 应该 fail
	if _, err := provider.Resolve(context.Background(), &repo.Credentials{APIKey: "sk-bogus"}); err == nil {
		t.Error("Resolve bogus key should fail")
	}
}

// TestEndToEnd_RevokedKeyRejected 验证 Revoke 后 gateway 拒绝该 key。
func TestEndToEnd_RevokedKeyRejected(t *testing.T) {
	engine := newTestEngine(t)

	w := do(t, engine, "POST", "/admin/v1/apikeys", apiKeyCreateRequest{UserID: "alice"})
	var created apiKeyCreateResponse
	_ = json.Unmarshal(w.Body.Bytes(), &created)

	dsn := os.Getenv("MYSQL_DSN")
	sqldb, _ := infra.Open(infra.DBConfig{Driver: infra.DriverMySQL, DSN: dsn})
	t.Cleanup(func() { _ = sqldb.Close() })
	provider := repo.NewSQLAPIKeyProvider(sqldb)

	// before revoke: works
	if _, err := provider.Resolve(context.Background(), &repo.Credentials{APIKey: created.APIKey}); err != nil {
		t.Fatalf("pre-revoke Resolve: %v", err)
	}

	// revoke
	w = do(t, engine, "POST", "/admin/v1/apikeys/"+created.APIKeyID+"/revoke", nil)
	if w.Code != 200 {
		t.Fatalf("revoke: %d", w.Code)
	}

	// after revoke: rejected
	if _, err := provider.Resolve(context.Background(), &repo.Credentials{APIKey: created.APIKey}); err == nil {
		t.Error("post-revoke Resolve should fail")
	}
}

// 确保 fmt 被用上（避免某次重构后失误删了）
var _ = fmt.Sprintf
