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

	"github.com/zereker/llm-gateway/internal/builtin"
	"github.com/zereker/llm-gateway/internal/cachebus"
	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/endpointcheck"
	"github.com/zereker/llm-gateway/internal/infra"
	"github.com/zereker/llm-gateway/internal/repo"
	"github.com/zereker/llm-gateway/internal/routingpolicy"
)

const (
	testToken   = "admin-secret-token"
	testDataKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// newTestEngine spins up a control-plane engine connected to a real MySQL;
// skips outright if MYSQL_DSN isn't set.
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
	// Truncate tables (in FK order)
	if _, err := db.Exec(`SET FOREIGN_KEY_CHECKS = 0`); err != nil {
		t.Fatalf("fk off: %v", err)
	}
	for _, table := range []string{
		"pricing_versions", "routing_cost_profiles", "routing_policies", "account_model_subscriptions", "api_keys",
		"endpoints", "model_services", "accounts", "quota_policies",
	} {
		if _, err := db.Exec("TRUNCATE TABLE " + table); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
	if _, err := db.Exec(`SET FOREIGN_KEY_CHECKS = 1`); err != nil {
		t.Fatalf("fk on: %v", err)
	}

	return NewEngine(newTestStore(db), []Token{{Value: testToken, Role: RoleAdmin}}), db
}

func TestConsoleRoutingPolicyLifecycleAndDryRun(t *testing.T) {
	engine, _ := newTestEngine(t)

	if code, body := do(t, engine, "POST", "/admin/accounts", AccountInput{Pin: "a1", Name: "Account 1"}, true); code != 201 {
		t.Fatalf("create account: code=%d body=%v", code, body)
	}
	_, first := do(t, engine, "POST", "/admin/model-services", ModelServiceInput{ServiceID: "local/small", Model: "small"}, true)
	modelID := int64(first["id"].(float64))
	if code, body := do(t, engine, "POST", "/admin/subscriptions", SubscriptionInput{AccountID: "a1", ModelServiceID: modelID}, true); code != 201 {
		t.Fatalf("subscribe: code=%d body=%v", code, body)
	}
	code, cost := do(t, engine, "POST", "/admin/routing-costs", RoutingCostInput{
		ModelServiceID: modelID, InputMicrousdPerMillionToken: 100, OutputMicrousdPerMillionToken: 200,
	}, true)
	if code != 201 || cost["routing_cost"].(map[string]any)["version"].(float64) != 1 {
		t.Fatalf("publish routing cost: code=%d body=%v", code, cost)
	}

	policy := RoutingPolicyInput{
		Scope:        domain.RoutingScope{Kind: domain.RoutingScopeAccount, ID: "a1"},
		VirtualModel: "fast-chat", MaxAttempts: 2,
		Candidates: []domain.RoutingPolicyCandidate{{Model: "small", Weight: 100}},
		Objectives: domain.RoutingObjectives{LatencyWeight: 2, CostWeight: 1, TargetLatencyMs: 100,
			TargetCostMicrousd: 1, EstimatedInputTokens: 1000, EstimatedOutputTokens: 500,
			MinTelemetrySamples: 1, TelemetryMaxAgeSeconds: 60},
	}
	code, created := do(t, engine, "POST", "/admin/routing-policies", policy, true)
	if code != 201 {
		t.Fatalf("publish: code=%d body=%v", code, created)
	}
	view := created["routing_policy"].(map[string]any)
	policyID := view["policy_id"].(string)
	if view["version"].(float64) != 1 {
		t.Fatalf("created policy=%v", view)
	}

	policy.PolicyID = policyID
	policy.MaxAttempts = 1
	code, created = do(t, engine, "POST", "/admin/routing-policies", policy, true)
	if code != 201 || created["routing_policy"].(map[string]any)["version"].(float64) != 2 {
		t.Fatalf("publish v2: code=%d body=%v", code, created)
	}

	chat := domain.ModalityChat
	code, dryRun := do(t, engine, "POST", "/admin/routing-policies/dry-run", RoutingDryRunInput{
		AccountID: "a1", RequestedModel: "fast-chat", Modality: &chat,
		Telemetry: map[string][]routingpolicy.EndpointTelemetry{"small": {{
			LatencyMs: 80, SuccessRate: 1, SampleCount: 10, Updated: time.Now().UTC(),
		}}},
	}, true)
	if code != 200 {
		t.Fatalf("dry-run: code=%d body=%v", code, dryRun)
	}
	chain := dryRun["model_chain"].([]any)
	if len(chain) != 1 || chain[0] != "small" {
		t.Fatalf("dry-run chain=%v", chain)
	}
	decision := dryRun["decision"].(map[string]any)
	score := decision["candidates"].([]any)[0].(map[string]any)["score"].(map[string]any)
	if score["latency_source"] != "observed" || score["cost_source"] != "configured" {
		t.Fatalf("dry-run score=%v", score)
	}
	if code, listedCosts := do(t, engine, "GET", "/admin/routing-costs", nil, true); code != 200 || len(listedCosts["routing_costs"].([]any)) != 1 {
		t.Fatalf("list routing costs: code=%d body=%v", code, listedCosts)
	}

	code, listed := do(t, engine, "GET", "/admin/routing-policies", nil, true)
	if code != 200 || len(listed["routing_policies"].([]any)) != 2 {
		t.Fatalf("list: code=%d body=%v", code, listed)
	}
	if code, body := do(t, engine, "DELETE", "/admin/routing-policies/"+policyID, nil, true); code != 200 {
		t.Fatalf("disable: code=%d body=%v", code, body)
	}
}

// newTestStore mirrors cmd/console's production wiring: the endpoint validator
// gets its protocol catalog from the built-in lookup.
func newTestStore(db *sqlx.DB) *Store {
	return NewStore(db).WithEndpointValidator(endpointcheck.Validator{Catalog: builtin.NewLookup()})
}

// do sends a JSON request with the admin token, returning the code + parsed body map.
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

// TestConsole_AuthRequired: no token / wrong token -> 401; the ops route is let through.
func TestConsole_AuthRequired(t *testing.T) {
	engine, _ := newTestEngine(t)

	if code, _ := do(t, engine, "GET", "/admin/accounts", nil, false); code != 401 {
		t.Errorf("no-token GET /admin/accounts = %d, want 401", code)
	}
	// wrong token
	req := httptest.NewRequest("GET", "/admin/accounts", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Errorf("wrong token = %d, want 401", w.Code)
	}
	// ops route is public
	if code, _ := do(t, engine, "GET", "/healthz", nil, false); code != 200 {
		t.Errorf("GET /healthz = %d, want 200", code)
	}
}

// TestConsole_EndpointCrossPlaneContract is the most critical regression for
// this split: an endpoint **written** by the control plane (KEK-encrypted
// credentials) must be **readable** by the data plane's repo reader and
// decryptable back to the original secret — proving the two planes' shared
// secret_crypto contract doesn't drift.
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

	// Data plane reader reads it back + decrypts (cross-plane contract verification)
	reader := repo.NewSQLEndpointReader(db)
	ep, err := reader.PickForModel(context.Background(), "gpt-4o", "default")
	if err != nil {
		t.Fatalf("gateway reader PickForModel: %v", err)
	}
	bearer, err := repo.DecodePayload[repo.BearerAuth](ep.Auth)
	if err != nil {
		t.Fatalf("decode bearer (encryption contract drift?): %v", err)
	}
	if bearer.APIKey != "sk-secret-upstream" {
		t.Errorf("decrypted upstream key = %q, want sk-secret-upstream", bearer.APIKey)
	}

	// The LIST view must never contain the secret
	code, list := do(t, engine, "GET", "/admin/endpoints", nil, true)
	if code != 200 {
		t.Fatalf("list = %d", code)
	}
	if bytes.Contains([]byte(toJSON(list)), []byte("sk-secret-upstream")) {
		t.Error("endpoint LIST leaked the upstream secret!")
	}
}

// TestConsole_EndpointValidationRejectsMetadata: pre-write validation blocks an SSRF metadata URL.
func TestConsole_EndpointValidationRejectsMetadata(t *testing.T) {
	engine, _ := newTestEngine(t)
	body := EndpointInput{
		Name: "evil", Vendor: "openai", Protocol: "openai", Model: "m",
		Auth:    AuthInput{Type: "bearer", Payload: json.RawMessage(`{"api_key":"x"}`)},
		Routing: repo.RoutingConfig{URL: "http://169.254.169.254/latest/meta-data/"},
	}
	code, resp := do(t, engine, "POST", "/admin/endpoints", body, true)
	if code != 400 {
		t.Fatalf("metadata URL should be 400, got %d resp=%v", code, resp)
	}
}

// TestConsole_APIKeyCrossPlaneLifecycle: control plane issues a key -> data
// plane resolver recognizes it -> control plane revokes -> data plane
// resolver rejects it. Issuance/recognition share the HashAPIKey contract.
func TestConsole_APIKeyCrossPlaneLifecycle(t *testing.T) {
	engine, db := newTestEngine(t)

	// Create the primary account first (FK)
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
		t.Fatalf("issue key response missing api_key/api_key_id: %v", resp)
	}

	// The data plane resolver recognizes this plaintext key (shared HashAPIKey)
	provider := repo.NewSQLAPIKeyProvider(db)
	id, err := provider.Resolve(context.Background(), &repo.Credentials{APIKey: plain})
	if err != nil {
		t.Fatalf("gateway resolver failed to recognize the new key (hash contract drift?): %v", err)
	}
	if id.SubAccountID != "alice" {
		t.Errorf("resolved sub_account = %q, want alice", id.SubAccountID)
	}

	// after revocation the resolver rejects it
	if code, _ := do(t, engine, "DELETE", "/admin/accounts/default/api-keys/"+keyID, nil, true); code != 200 {
		t.Fatalf("revoke = %d", code)
	}
	if _, err := provider.Resolve(context.Background(), &repo.Credentials{APIKey: plain}); !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Errorf("resolve err after revocation = %v, want ErrInvalidCredentials", err)
	}
}

// TestConsole_RevokeEvictsDataPlaneCache is the end-to-end regression for
// Phase 1: when the control plane revokes a key, it publishes an
// invalidation via cachebus -> the data plane's subscribed
// CachedAPIKeyProvider evicts precisely. This proves "revocation takes
// effect immediately" (no need to wait for TTL). Requires MYSQL_DSN + REDIS_ADDR.
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

	// Data plane side: cached provider + subscribed to evict.
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

	// Control plane side: a store with a publisher (reusing the same engine
	// through the API would be more realistic, but here we trigger revoke
	// directly via the store to focus on the cachebus loop).
	store := newTestStore(db).WithPublisher(cachebus.NewPublisher(rdb, channel))
	api := NewEngine(store, []Token{{Value: testToken, Role: RoleAdmin}})

	// Create account + issue key.
	if code, resp := do(t, engine, "POST", "/admin/accounts", AccountInput{Pin: "default", Name: "D"}, true); code != 201 {
		t.Fatalf("create account = %d %v", code, resp)
	}
	_, resp := do(t, engine, "POST", "/admin/api-keys", APIKeyInput{AccountID: "default", SubAccountID: "eve"}, true)
	plain, _ := resp["api_key"].(string)
	keyID, _ := resp["api_key_id"].(string)

	// Data plane resolves once -> valid, enters the positive cache (30s TTL).
	if _, err := provider.Resolve(context.Background(), &repo.Credentials{APIKey: plain}); err != nil {
		t.Fatalf("initial resolve: %v", err)
	}

	// Control plane revokes (via the store with a publisher) -> DB is set to
	// revoked + a cachebus invalidation is published.
	if code, _ := doOn(t, api, "DELETE", "/admin/accounts/default/api-keys/"+keyID, nil, true); code != 200 {
		t.Fatalf("revoke via store-with-publisher failed")
	}

	// Wait for the data plane to receive the evict notification.
	select {
	case <-evicted:
	case <-time.After(3 * time.Second):
		t.Fatal("data plane did not receive the evict notification")
	}

	// Key assertion: resolving right after eviction should be a 401 (the
	// cache is cleared, so it re-queries the DB and sees revoked) — without
	// cachebus, this would return the **stale valid identity from the
	// cache** for up to 30s TTL.
	if _, err := provider.Resolve(context.Background(), &repo.Credentials{APIKey: plain}); !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Errorf("resolve err after evict = %v, want ErrInvalidCredentials (revocation should take effect immediately)", err)
	}
}

// TestConsole_ViewerRoleReadOnly: a viewer token can GET but not POST/DELETE (403).
func TestConsole_ViewerRoleReadOnly(t *testing.T) {
	_, db := newTestEngine(t)
	const viewerTok = "viewer-only-token"
	engine := NewEngine(newTestStore(db), []Token{
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

	// viewer read OK
	if code := req("GET", "/admin/endpoints", viewerTok); code != 200 {
		t.Errorf("viewer GET /admin/endpoints = %d, want 200", code)
	}
	// viewer write -> 403
	if code := req("POST", "/admin/accounts", viewerTok); code != 403 {
		t.Errorf("viewer POST /admin/accounts = %d, want 403", code)
	}
	if code := req("DELETE", "/admin/endpoints/1", viewerTok); code != 403 {
		t.Errorf("viewer DELETE = %d, want 403", code)
	}
	// admin write OK
	if code := req("POST", "/admin/accounts", testToken); code != 201 {
		t.Errorf("admin POST /admin/accounts = %d, want 201", code)
	}
}

// TestConsole_ModelAliasCrossPlane: control plane creates an alias -> data
// plane reader resolves the alias to the canonical model_service
// (cross-plane); creating an alias pointing at a nonexistent model -> 400;
// after deletion, resolution misses.
func TestConsole_ModelAliasCrossPlane(t *testing.T) {
	engine, db := newTestEngine(t)

	// canonical model
	if code, _ := do(t, engine, "POST", "/admin/model-services",
		ModelServiceInput{ServiceID: "openai/gpt-4o-mini", Model: "gpt-4o-mini"}, true); code != 201 {
		t.Fatal("create model")
	}
	// alias fast -> gpt-4o-mini
	if code, resp := do(t, engine, "POST", "/admin/model-aliases",
		ModelAliasInput{Alias: "fast", Model: "gpt-4o-mini"}, true); code != 201 {
		t.Fatalf("create alias = %d %v", code, resp)
	}
	// pointing at a nonexistent model -> 400
	if code, _ := do(t, engine, "POST", "/admin/model-aliases",
		ModelAliasInput{Alias: "bad", Model: "no-such-model"}, true); code != 400 {
		t.Errorf("dead alias = %d, want 400", code)
	}

	reader := repo.NewSQLModelServiceReader(db)
	// alias resolves to canonical
	ms, err := reader.GetByModel(context.Background(), "fast")
	if err != nil || ms == nil || ms.Model != "gpt-4o-mini" {
		t.Fatalf(`GetByModel("fast") = %v, %v; want canonical gpt-4o-mini`, ms, err)
	}
	// direct lookup of canonical still works
	if ms2, _ := reader.GetByModel(context.Background(), "gpt-4o-mini"); ms2 == nil || ms2.Model != "gpt-4o-mini" {
		t.Errorf("direct lookup broken: %v", ms2)
	}
	// unknown name -> (nil, nil)
	if ms3, err := reader.GetByModel(context.Background(), "totally-unknown"); ms3 != nil || err != nil {
		t.Errorf(`GetByModel(unknown) = %v, %v; want nil,nil`, ms3, err)
	}

	// after deleting the alias, resolution misses
	if code, _ := do(t, engine, "DELETE", "/admin/model-aliases/fast", nil, true); code != 200 {
		t.Fatal("delete alias")
	}
	if ms4, _ := reader.GetByModel(context.Background(), "fast"); ms4 != nil {
		t.Errorf("GetByModel(fast) still resolved to %v after deleting the alias", ms4)
	}
}

// TestConsole_QuotaPolicyCRUD: create (validating rule_json) + list + delete.
func TestConsole_QuotaPolicyCRUD(t *testing.T) {
	engine, _ := newTestEngine(t)

	// missing name -> 400
	if code, _ := do(t, engine, "POST", "/admin/quota-policies",
		QuotaPolicyInput{Rule: json.RawMessage(`{"default":{"rpm":60}}`)}, true); code != 400 {
		t.Errorf("missing name = %d, want 400", code)
	}
	// empty policy (no default/per_model) -> 400
	if code, _ := do(t, engine, "POST", "/admin/quota-policies",
		QuotaPolicyInput{Name: "empty", Rule: json.RawMessage(`{}`)}, true); code != 400 {
		t.Errorf("empty rule = %d, want 400", code)
	}
	// valid -> 201
	code, resp := do(t, engine, "POST", "/admin/quota-policies",
		QuotaPolicyInput{Name: "tier1", Description: "60rpm", Rule: json.RawMessage(`{"default":{"rpm":60,"tpm":100000},"per_model":{"gpt-4o":{"rpm":10}}}`)}, true)
	if code != 201 {
		t.Fatalf("create policy = %d resp=%v", code, resp)
	}
	id := int64(resp["id"].(float64))

	// list includes tier1
	code, list := do(t, engine, "GET", "/admin/quota-policies", nil, true)
	if code != 200 {
		t.Fatalf("list = %d", code)
	}
	if !bytes.Contains([]byte(toJSON(list)), []byte(`"tier1"`)) {
		t.Errorf("list does not contain tier1: %v", list)
	}

	// delete
	if code, _ := do(t, engine, "DELETE", "/admin/quota-policies/"+itoa(id), nil, true); code != 200 {
		t.Errorf("delete = %d, want 200", code)
	}
}

// TestConsole_PricingAppendOnly: publishing a second price version closes out
// the first one (effective_to gets set), leaving only one active version.
func TestConsole_PricingAppendOnly(t *testing.T) {
	engine, db := newTestEngine(t)
	// Prerequisite: account + model_service (FK)
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

	// Exactly one active row (effective_to IS NULL)
	var active int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pricing_versions WHERE account_id='default' AND model_service_id=? AND effective_to IS NULL`, msID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Errorf("active version count = %d, want 1 (append-only should leave only one active)", active)
	}
	// Two versions total (v1 closed out, v2 active)
	code, list := do(t, engine, "GET", "/admin/pricing?account_id=default", nil, true)
	if code != 200 {
		t.Fatal("list pricing")
	}
	arr, _ := list["pricing"].([]any)
	if len(arr) != 2 {
		t.Errorf("total version count = %d, want 2", len(arr))
	}
	// active-only filter should return just 1 row
	_, list = do(t, engine, "GET", "/admin/pricing?account_id=default&active=true", nil, true)
	arr, _ = list["pricing"].([]any)
	if len(arr) != 1 {
		t.Errorf("active-only version count = %d, want 1", len(arr))
	}
}

// TestConsole_AuditLog: write operations are audited (including
// actor/method/path), audit is admin-only, and the audit entry excludes the
// request body (secrets don't land in the audit log).
func TestConsole_AuditLog(t *testing.T) {
	_, db := newTestEngine(t)
	if _, err := db.Exec("TRUNCATE TABLE audit_log"); err != nil {
		t.Fatalf("truncate audit_log: %v", err)
	}
	engine := NewEngine(newTestStore(db), []Token{
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

	// admin creates an endpoint (body contains the upstream secret)
	secret := `{"name":"e","vendor":"openai","protocol":"openai","model":"m","auth":{"type":"bearer","payload":{"api_key":"sk-should-not-be-audited"}},"routing":{"url":"https://api.openai.com/v1"}}`
	if code, _ := send("POST", "/admin/endpoints", testToken, secret); code != 201 {
		t.Fatalf("create endpoint = %d", code)
	}

	// admin reads the audit log
	code, body := send("GET", "/admin/audit", testToken, "")
	if code != 200 {
		t.Fatalf("audit read = %d", code)
	}
	if !bytes.Contains(body, []byte(`"alice-admin"`)) || !bytes.Contains(body, []byte(`/admin/endpoints`)) || !bytes.Contains(body, []byte(`"POST"`)) {
		t.Errorf("audit log missing entry: %s", body)
	}
	// Key point: the audit entry must never contain the upstream secret
	if bytes.Contains(body, []byte("sk-should-not-be-audited")) {
		t.Error("audit log leaked the secret from the request body!")
	}
	// viewer cannot view the audit log
	if code, _ := send("GET", "/admin/audit", "viewer-tok", ""); code != 403 {
		t.Errorf("viewer GET /admin/audit = %d, want 403", code)
	}
}

// TestConsole_PricingConcurrentSingleActive: concurrent publishes for the
// same (account,model,class) leave only one active row — a regression for
// GET_LOCK serialization.
func TestConsole_PricingConcurrentSingleActive(t *testing.T) {
	engine, db := newTestEngine(t)
	if code, _ := do(t, engine, "POST", "/admin/accounts", AccountInput{Pin: "default", Name: "D"}, true); code != 201 {
		t.Fatal("account")
	}
	_, resp := do(t, engine, "POST", "/admin/model-services", ModelServiceInput{ServiceID: "o/m", Model: "cm"}, true)
	msID := int64(resp["id"].(float64))

	store := newTestStore(db)
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
		t.Errorf("active row count after concurrent publish = %d, want 1 (GET_LOCK should serialize)", active)
	}
}

// TestConsole_FKViolationIs400: referencing a nonexistent resource (FK 1452) -> 400, not 500.
func TestConsole_FKViolationIs400(t *testing.T) {
	engine, _ := newTestEngine(t)
	if code, _ := do(t, engine, "POST", "/admin/accounts", AccountInput{Pin: "default", Name: "D"}, true); code != 201 {
		t.Fatal("account")
	}
	// subscribe to a nonexistent model_service_id -> FK failure -> 400
	code, _ := do(t, engine, "POST", "/admin/subscriptions",
		SubscriptionInput{AccountID: "default", ModelServiceID: 999999}, true)
	if code != 400 {
		t.Errorf("subscribe to nonexistent model = %d, want 400 (invalid_reference)", code)
	}
}

func ftoa(f float64) string {
	// Only used for the fixed test values 5.0/6.0
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

// doOn is like do but sends the request to the given engine.
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
