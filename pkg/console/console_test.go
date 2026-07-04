package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/infra"
	"github.com/zereker/llm-gateway/pkg/repo"

	// endpointcheck 的 vendor / translator 注册（否则合法 endpoint 被误判）
	_ "github.com/zereker/llm-gateway/pkg/protocol/anthropic"
	_ "github.com/zereker/llm-gateway/pkg/protocol/gemini"
	_ "github.com/zereker/llm-gateway/pkg/protocol/openai"
	_ "github.com/zereker/llm-gateway/pkg/translator/identity"
	_ "github.com/zereker/llm-gateway/pkg/translator/openai_anthropic"
)

const (
	testToken   = "admin-secret-token"
	testDataKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// newTestEngine 起一个连真实 MySQL 的控制面 engine；没设 MYSQL_DSN 直接 skip。
func newTestEngine(t *testing.T) (*gin.Engine, *sqlx.DB) {
	t.Helper()
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("MYSQL_DSN not set; skipping console integration test")
	}
	gin.SetMode(gin.TestMode)
	if err := repo.SetDataKey(testDataKey); err != nil {
		t.Fatalf("SetDataKey: %v", err)
	}
	db, err := infra.Open(infra.DBConfig{Driver: infra.DriverMySQL, DSN: dsn})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	if err := infra.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// 清表（FK 顺序）
	if _, err := db.Exec(`SET FOREIGN_KEY_CHECKS = 0`); err != nil {
		t.Fatalf("fk off: %v", err)
	}
	for _, table := range []string{
		"pricing_versions", "account_model_subscriptions", "api_keys",
		"endpoints", "model_services", "accounts", "quota_policies",
	} {
		if _, err := db.Exec("TRUNCATE TABLE " + table); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
	if _, err := db.Exec(`SET FOREIGN_KEY_CHECKS = 1`); err != nil {
		t.Fatalf("fk on: %v", err)
	}

	return NewEngine(NewStore(db), []string{testToken}), db
}

// do 发一条带 admin token 的 JSON 请求，返回 code + 解析后的 body map。
func do(t *testing.T, engine *gin.Engine, method, path string, body any, withAuth bool) (int, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if withAuth {
		req.Header.Set("Authorization", "Bearer "+testToken)
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// TestConsole_AuthRequired：无 token / 错 token → 401；对 ops 路由放行。
func TestConsole_AuthRequired(t *testing.T) {
	engine, _ := newTestEngine(t)

	if code, _ := do(t, engine, "GET", "/admin/accounts", nil, false); code != 401 {
		t.Errorf("无 token GET /admin/accounts = %d, want 401", code)
	}
	// 错 token
	req := httptest.NewRequest("GET", "/admin/accounts", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Errorf("错 token = %d, want 401", w.Code)
	}
	// ops 路由公开
	if code, _ := do(t, engine, "GET", "/healthz", nil, false); code != 200 {
		t.Errorf("GET /healthz = %d, want 200", code)
	}
}

// TestConsole_EndpointCrossPlaneContract 是本次拆分最关键的回归：
// 控制面**写**的 endpoint（KEK 加密凭证），数据面的 repo reader 必须能**读**出来
// 并解密回原始密钥——证明两个面共享 secret_crypto 契约不漂移。
func TestConsole_EndpointCrossPlaneContract(t *testing.T) {
	engine, db := newTestEngine(t)

	body := EndpointInput{
		Name: "openai_main", Vendor: "openai", Protocol: "openai", Model: "gpt-4o",
		Auth:    AuthInput{Type: "bearer", Payload: json.RawMessage(`{"api_key":"sk-secret-upstream"}`)},
		Routing: repo.RoutingConfig{URL: "https://api.openai.com/v1/chat/completions"},
	}
	code, resp := do(t, engine, "POST", "/admin/endpoints", body, true)
	if code != 201 {
		t.Fatalf("create endpoint = %d, resp=%v", code, resp)
	}

	// 数据面 reader 读回来 + 解密（跨面契约验证）
	reader := repo.NewSQLEndpointReader(db)
	ep, err := reader.PickForModel(context.Background(), "gpt-4o", "default")
	if err != nil {
		t.Fatalf("gateway reader PickForModel: %v", err)
	}
	bearer, err := repo.DecodePayload[repo.BearerAuth](ep.Auth)
	if err != nil {
		t.Fatalf("decode bearer (加密契约漂移?): %v", err)
	}
	if bearer.APIKey != "sk-secret-upstream" {
		t.Errorf("解密出的上游 key = %q, want sk-secret-upstream", bearer.APIKey)
	}

	// LIST 视图绝不含密钥
	code, list := do(t, engine, "GET", "/admin/endpoints", nil, true)
	if code != 200 {
		t.Fatalf("list = %d", code)
	}
	if bytes.Contains([]byte(toJSON(list)), []byte("sk-secret-upstream")) {
		t.Error("endpoint LIST 泄漏了上游密钥！")
	}
}

// TestConsole_EndpointValidationRejectsMetadata：写前校验拦 SSRF metadata URL。
func TestConsole_EndpointValidationRejectsMetadata(t *testing.T) {
	engine, _ := newTestEngine(t)
	body := EndpointInput{
		Name: "evil", Vendor: "openai", Protocol: "openai", Model: "m",
		Auth:    AuthInput{Type: "bearer", Payload: json.RawMessage(`{"api_key":"x"}`)},
		Routing: repo.RoutingConfig{URL: "http://169.254.169.254/latest/meta-data/"},
	}
	code, resp := do(t, engine, "POST", "/admin/endpoints", body, true)
	if code != 400 {
		t.Fatalf("metadata URL 应 400, got %d resp=%v", code, resp)
	}
}

// TestConsole_APIKeyCrossPlaneLifecycle：控制面发 key → 数据面 resolver 认得 →
// 控制面吊销 → 数据面 resolver 拒。发/认共享 HashAPIKey 契约。
func TestConsole_APIKeyCrossPlaneLifecycle(t *testing.T) {
	engine, db := newTestEngine(t)

	// 先建主账号（FK）
	if code, resp := do(t, engine, "POST", "/admin/accounts",
		AccountInput{Pin: "default", Name: "Default"}, true); code != 201 {
		t.Fatalf("create account = %d resp=%v", code, resp)
	}

	code, resp := do(t, engine, "POST", "/admin/api-keys",
		APIKeyInput{AccountID: "default", SubAccountID: "alice", Name: "prod"}, true)
	if code != 201 {
		t.Fatalf("create key = %d resp=%v", code, resp)
	}
	plain, _ := resp["api_key"].(string)
	keyID, _ := resp["api_key_id"].(string)
	if plain == "" || keyID == "" {
		t.Fatalf("发 key 返回缺 api_key/api_key_id: %v", resp)
	}

	// 数据面 resolver 认得这把明文 key（共享 HashAPIKey）
	provider := repo.NewSQLAPIKeyProvider(db)
	id, err := provider.Resolve(context.Background(), &repo.Credentials{APIKey: plain})
	if err != nil {
		t.Fatalf("gateway resolver 认不出新 key (hash 契约漂移?): %v", err)
	}
	if id.SubAccountID != "alice" {
		t.Errorf("resolved sub_account = %q, want alice", id.SubAccountID)
	}

	// 吊销后 resolver 拒
	if code, _ := do(t, engine, "DELETE", "/admin/accounts/default/api-keys/"+keyID, nil, true); code != 200 {
		t.Fatalf("revoke = %d", code)
	}
	if _, err := provider.Resolve(context.Background(), &repo.Credentials{APIKey: plain}); !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Errorf("吊销后 resolve err = %v, want ErrInvalidCredentials", err)
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
