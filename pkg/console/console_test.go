package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/redis/go-redis/v9"

	"github.com/zereker/llm-gateway/pkg/cachebus"
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

	return NewEngine(NewStore(db), []Token{{Value: testToken, Role: RoleAdmin}}), db
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

// TestConsole_RevokeEvictsDataPlaneCache 是 Phase 1 的端到端回归：控制面吊销 key
// 时经 cachebus 发布失效 → 数据面订阅的 CachedAPIKeyProvider 精准 evict。证明
// "吊销即时生效"（不用等 TTL）。需 MYSQL_DSN + REDIS_ADDR。
func TestConsole_RevokeEvictsDataPlaneCache(t *testing.T) {
	engine, db := newTestEngine(t)
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR not set; skipping cachebus eviction test")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis ping failed (%v); skipping", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	channel := "test:console:" + t.Name()

	// 数据面侧：cached provider + 订阅 evict。
	provider := repo.NewCachedAPIKeyProvider(repo.NewSQLAPIKeyProvider(db), 1024, 30*time.Second, nil)
	evicted := make(chan string, 1)
	sub := cachebus.NewSubscriber(rdb, channel, func(inv cachebus.Invalidation) {
		if inv.Kind == cachebus.KindAPIKey {
			provider.Evict(inv.Key)
			evicted <- inv.Key
		}
	})
	stop, err := sub.Start(context.Background())
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer stop()

	// 控制面侧：带 publisher 的 store（复用同一 engine 走 API 更真实，但这里直接
	// 用 store 触发 revoke 以聚焦 cachebus 环路）。
	store := NewStore(db).WithPublisher(cachebus.NewPublisher(rdb, channel))
	api := NewEngine(store, []Token{{Value: testToken, Role: RoleAdmin}})

	// 建账号 + 发 key。
	if code, resp := do(t, engine, "POST", "/admin/accounts", AccountInput{Pin: "default", Name: "D"}, true); code != 201 {
		t.Fatalf("create account = %d %v", code, resp)
	}
	_, resp := do(t, engine, "POST", "/admin/api-keys", APIKeyInput{AccountID: "default", SubAccountID: "eve"}, true)
	plain, _ := resp["api_key"].(string)
	keyID, _ := resp["api_key_id"].(string)

	// 数据面 resolve 一次 → valid，进正向缓存（30s TTL）。
	if _, err := provider.Resolve(context.Background(), &repo.Credentials{APIKey: plain}); err != nil {
		t.Fatalf("initial resolve: %v", err)
	}

	// 控制面吊销（经带 publisher 的 store）→ DB 置 revoked + 发布 cachebus 失效。
	if code, _ := doOn(t, api, "DELETE", "/admin/accounts/default/api-keys/"+keyID, nil, true); code != 200 {
		t.Fatalf("revoke via store-with-publisher failed")
	}

	// 等数据面收到 evict 通知。
	select {
	case <-evicted:
	case <-time.After(3 * time.Second):
		t.Fatal("数据面没收到 evict 通知")
	}

	// 关键断言：evict 后立刻 resolve 应 401（缓存已清，重查 DB 见 revoked）——
	// 若没有 cachebus，这里会返回**缓存里的旧 valid 身份**长达 30s TTL。
	if _, err := provider.Resolve(context.Background(), &repo.Credentials{APIKey: plain}); !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Errorf("evict 后 resolve err = %v, want ErrInvalidCredentials（吊销应即时生效）", err)
	}
}

// TestConsole_ViewerRoleReadOnly：viewer token 能 GET，不能 POST/DELETE（403）。
func TestConsole_ViewerRoleReadOnly(t *testing.T) {
	_, db := newTestEngine(t)
	const viewerTok = "viewer-only-token"
	engine := NewEngine(NewStore(db), []Token{
		{Value: testToken, Role: RoleAdmin},
		{Value: viewerTok, Role: RoleViewer},
	})

	req := func(method, path, tok string) int {
		r := httptest.NewRequest(method, path, bytes.NewReader([]byte(`{"pin":"x","name":"y"}`)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, r)
		return w.Code
	}

	// viewer 读 OK
	if code := req("GET", "/admin/endpoints", viewerTok); code != 200 {
		t.Errorf("viewer GET /admin/endpoints = %d, want 200", code)
	}
	// viewer 写 → 403
	if code := req("POST", "/admin/accounts", viewerTok); code != 403 {
		t.Errorf("viewer POST /admin/accounts = %d, want 403", code)
	}
	if code := req("DELETE", "/admin/endpoints/1", viewerTok); code != 403 {
		t.Errorf("viewer DELETE = %d, want 403", code)
	}
	// admin 写 OK
	if code := req("POST", "/admin/accounts", testToken); code != 201 {
		t.Errorf("admin POST /admin/accounts = %d, want 201", code)
	}
}

// TestConsole_ModelAliasCrossPlane：控制面建别名 → 数据面 reader 把 alias 解析成
// canonical model_service（跨面）；建指向不存在 model 的别名 → 400；删后解析 miss。
func TestConsole_ModelAliasCrossPlane(t *testing.T) {
	engine, db := newTestEngine(t)

	// canonical model
	if code, _ := do(t, engine, "POST", "/admin/model-services",
		ModelServiceInput{ServiceID: "openai/gpt-4o-mini", Model: "gpt-4o-mini"}, true); code != 201 {
		t.Fatal("create model")
	}
	// 别名 fast → gpt-4o-mini
	if code, resp := do(t, engine, "POST", "/admin/model-aliases",
		ModelAliasInput{Alias: "fast", Model: "gpt-4o-mini"}, true); code != 201 {
		t.Fatalf("create alias = %d %v", code, resp)
	}
	// 指向不存在的 model → 400
	if code, _ := do(t, engine, "POST", "/admin/model-aliases",
		ModelAliasInput{Alias: "bad", Model: "no-such-model"}, true); code != 400 {
		t.Errorf("dead alias = %d, want 400", code)
	}

	reader := repo.NewSQLModelServiceReader(db)
	// alias 解析到 canonical
	ms, err := reader.GetByModel(context.Background(), "fast")
	if err != nil || ms == nil || ms.Model != "gpt-4o-mini" {
		t.Fatalf(`GetByModel("fast") = %v, %v; want canonical gpt-4o-mini`, ms, err)
	}
	// 直查 canonical 仍然正常
	if ms2, _ := reader.GetByModel(context.Background(), "gpt-4o-mini"); ms2 == nil || ms2.Model != "gpt-4o-mini" {
		t.Errorf("direct lookup broken: %v", ms2)
	}
	// 未知名 → (nil, nil)
	if ms3, err := reader.GetByModel(context.Background(), "totally-unknown"); ms3 != nil || err != nil {
		t.Errorf(`GetByModel(unknown) = %v, %v; want nil,nil`, ms3, err)
	}

	// 删别名后解析 miss
	if code, _ := do(t, engine, "DELETE", "/admin/model-aliases/fast", nil, true); code != 200 {
		t.Fatal("delete alias")
	}
	if ms4, _ := reader.GetByModel(context.Background(), "fast"); ms4 != nil {
		t.Errorf("删别名后 GetByModel(fast) 仍解析出 %v", ms4)
	}
}

// TestConsole_QuotaPolicyCRUD：建（校验 rule_json）+ 列 + 删。
func TestConsole_QuotaPolicyCRUD(t *testing.T) {
	engine, _ := newTestEngine(t)

	// 缺 name → 400
	if code, _ := do(t, engine, "POST", "/admin/quota-policies",
		QuotaPolicyInput{Rule: json.RawMessage(`{"default":{"rpm":60}}`)}, true); code != 400 {
		t.Errorf("缺 name = %d, want 400", code)
	}
	// 空策略（无 default/per_model）→ 400
	if code, _ := do(t, engine, "POST", "/admin/quota-policies",
		QuotaPolicyInput{Name: "empty", Rule: json.RawMessage(`{}`)}, true); code != 400 {
		t.Errorf("空 rule = %d, want 400", code)
	}
	// 合法 → 201
	code, resp := do(t, engine, "POST", "/admin/quota-policies",
		QuotaPolicyInput{Name: "tier1", Description: "60rpm", Rule: json.RawMessage(`{"default":{"rpm":60,"tpm":100000},"per_model":{"gpt-4o":{"rpm":10}}}`)}, true)
	if code != 201 {
		t.Fatalf("create policy = %d resp=%v", code, resp)
	}
	id := int64(resp["id"].(float64))

	// list 含 tier1
	code, list := do(t, engine, "GET", "/admin/quota-policies", nil, true)
	if code != 200 {
		t.Fatalf("list = %d", code)
	}
	if !bytes.Contains([]byte(toJSON(list)), []byte(`"tier1"`)) {
		t.Errorf("list 未含 tier1: %v", list)
	}

	// delete
	if code, _ := do(t, engine, "DELETE", "/admin/quota-policies/"+itoa(id), nil, true); code != 200 {
		t.Errorf("delete = %d, want 200", code)
	}
}

// TestConsole_PricingAppendOnly：发布第二版价格会封盘第一版（effective_to 置值），
// 且只剩一个 active。
func TestConsole_PricingAppendOnly(t *testing.T) {
	engine, db := newTestEngine(t)
	// 前置：account + model_service（FK）
	if code, _ := do(t, engine, "POST", "/admin/accounts", AccountInput{Pin: "default", Name: "D"}, true); code != 201 {
		t.Fatal("account")
	}
	_, resp := do(t, engine, "POST", "/admin/model-services", ModelServiceInput{ServiceID: "openai/gpt-4o", Model: "gpt-4o"}, true)
	msID := int64(resp["id"].(float64))

	pub := func(rate float64) int {
		code, _ := do(t, engine, "POST", "/admin/pricing", PricingInput{
			AccountID: "default", ModelServiceID: msID,
			Rule:      json.RawMessage(`{"unit":"tokens_per_1m","currency":"USD","rates":{"input":` + ftoa(rate) + `}}`),
			CreatedBy: "test",
		}, true)
		return code
	}
	if pub(5.0) != 201 {
		t.Fatal("publish v1")
	}
	if pub(6.0) != 201 {
		t.Fatal("publish v2")
	}

	// 恰好一个 active（effective_to IS NULL）
	var active int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pricing_versions WHERE account_id='default' AND model_service_id=? AND effective_to IS NULL`, msID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Errorf("active 版本数 = %d, want 1（append-only 应只留一个 active）", active)
	}
	// 共两个版本（v1 被封盘、v2 active）
	code, list := do(t, engine, "GET", "/admin/pricing?account_id=default", nil, true)
	if code != 200 {
		t.Fatal("list pricing")
	}
	arr, _ := list["pricing"].([]any)
	if len(arr) != 2 {
		t.Errorf("版本总数 = %d, want 2", len(arr))
	}
	// active-only 过滤应只回 1 条
	_, list = do(t, engine, "GET", "/admin/pricing?account_id=default&active=true", nil, true)
	arr, _ = list["pricing"].([]any)
	if len(arr) != 1 {
		t.Errorf("active-only 版本数 = %d, want 1", len(arr))
	}
}

// TestConsole_AuditLog：写操作被审计（含 actor/method/path），审计只给 admin 看，
// 且审计不含 request body（密钥不落审计）。
func TestConsole_AuditLog(t *testing.T) {
	_, db := newTestEngine(t)
	if _, err := db.Exec("TRUNCATE TABLE audit_log"); err != nil {
		t.Fatalf("truncate audit_log: %v", err)
	}
	engine := NewEngine(NewStore(db), []Token{
		{Value: testToken, Role: RoleAdmin, Name: "alice-admin"},
		{Value: "viewer-tok", Role: RoleViewer, Name: "bob"},
	})
	send := func(method, path, tok, body string) (int, []byte) {
		r := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
		r.Header.Set("Content-Type", "application/json")
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, r)
		return w.Code, w.Body.Bytes()
	}

	// admin 建 endpoint（body 含上游密钥）
	secret := `{"name":"e","vendor":"openai","protocol":"openai","model":"m","auth":{"type":"bearer","payload":{"api_key":"sk-should-not-be-audited"}},"routing":{"url":"https://api.openai.com/v1"}}`
	if code, _ := send("POST", "/admin/endpoints", testToken, secret); code != 201 {
		t.Fatalf("create endpoint = %d", code)
	}

	// admin 读审计
	code, body := send("GET", "/admin/audit", testToken, "")
	if code != 200 {
		t.Fatalf("audit read = %d", code)
	}
	if !bytes.Contains(body, []byte(`"alice-admin"`)) || !bytes.Contains(body, []byte(`/admin/endpoints`)) || !bytes.Contains(body, []byte(`"POST"`)) {
		t.Errorf("审计缺条目: %s", body)
	}
	// 关键：审计里绝不含上游密钥
	if bytes.Contains(body, []byte("sk-should-not-be-audited")) {
		t.Error("审计泄漏了 request body 里的密钥！")
	}
	// viewer 不能看审计
	if code, _ := send("GET", "/admin/audit", "viewer-tok", ""); code != 403 {
		t.Errorf("viewer GET /admin/audit = %d, want 403", code)
	}
}

// TestConsole_PricingConcurrentSingleActive：并发发布同 (account,model,class) 只留
// 一条 active——GET_LOCK 串行化的回归。
func TestConsole_PricingConcurrentSingleActive(t *testing.T) {
	engine, db := newTestEngine(t)
	if code, _ := do(t, engine, "POST", "/admin/accounts", AccountInput{Pin: "default", Name: "D"}, true); code != 201 {
		t.Fatal("account")
	}
	_, resp := do(t, engine, "POST", "/admin/model-services", ModelServiceInput{ServiceID: "o/m", Model: "cm"}, true)
	msID := int64(resp["id"].(float64))

	store := NewStore(db)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, _ = store.PublishPrice(context.Background(), PricingInput{
				AccountID: "default", ModelServiceID: msID,
				Rule: json.RawMessage(`{"rates":{"input":` + itoa(int64(n)) + `}}`),
			})
		}(i)
	}
	wg.Wait()

	var active int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pricing_versions WHERE account_id='default' AND model_service_id=? AND effective_to IS NULL`, msID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Errorf("并发发布后 active 行数 = %d, want 1（GET_LOCK 应串行化）", active)
	}
}

// TestConsole_FKViolationIs400：引用不存在资源（FK 1452）→ 400 而非 500。
func TestConsole_FKViolationIs400(t *testing.T) {
	engine, _ := newTestEngine(t)
	if code, _ := do(t, engine, "POST", "/admin/accounts", AccountInput{Pin: "default", Name: "D"}, true); code != 201 {
		t.Fatal("account")
	}
	// 订阅一个不存在的 model_service_id → FK 失败 → 400
	code, _ := do(t, engine, "POST", "/admin/subscriptions",
		SubscriptionInput{AccountID: "default", ModelServiceID: 999999}, true)
	if code != 400 {
		t.Errorf("订阅不存在的 model = %d, want 400（invalid_reference）", code)
	}
}

func ftoa(f float64) string {
	// 只用于测试固定值 5.0/6.0
	return itoa(int64(f)) + ".0"
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// doOn 跟 do 一样但对指定 engine 发请求。
func doOn(t *testing.T, engine *gin.Engine, method, path string, body any, withAuth bool) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if withAuth {
		req.Header.Set("Authorization", "Bearer "+testToken)
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
