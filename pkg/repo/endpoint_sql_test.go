package repo

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jmoiron/sqlx"
)

// seedEndpoint 用 NamedExec 插测试 endpoint。
//
// 走 NamedExec 是为了让 Auth/Routing/Quota/Capabilities 字段经 Valuer 接口
// 转 JSON / 加密——raw INSERT 字符串拿不到这层魔法。
//
// **v0.3 改动**：endpoint 表去掉 account_id（全局上游池）。
func seedEndpoint(t *testing.T, db *sqlx.DB, ep *Endpoint) {
	t.Helper()
	if ep.Group == "" {
		ep.Group = "default"
	}
	if ep.Weight == 0 {
		ep.Weight = 100
	}
	ep.Enabled = true
	if ep.Protocol == "" {
		ep.Protocol = "openai" // 默认值；测试不关心协议细节时省略
	}
	if ep.Auth.Type == "" {
		auth, err := EncodePayload(AuthTypeBearer, BearerAuth{APIKey: "sk-test"})
		if err != nil {
			t.Fatalf("encode bearer: %v", err)
		}
		ep.Auth = auth
	}
	if (ep.Routing == RoutingConfig{}) {
		ep.Routing = RoutingConfig{URL: "https://invoker.test/v1/chat"}
	}
	res, err := db.NamedExec(
		`INSERT INTO endpoints
		 (name, vendor, protocol, model, group_name, weight, enabled,
		  auth, routing, quota, capabilities, quirks, extra)
		 VALUES
		 (:name, :vendor, :protocol, :model, :group_name, :weight, :enabled,
		  :auth, :routing, :quota, :capabilities, :quirks, :extra)`,
		ep,
	)
	if err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}
	if id, err := res.LastInsertId(); err == nil {
		ep.ID = id
	}
}

func TestSQLEndpointReader_PickForModel(t *testing.T) {
	db := newTestDB(t)
	ep := &Endpoint{
		Name:         "openai_main",
		Vendor:       "openai",
		Model:        "gpt-4o",
		Group:        "default",
		Weight:       100,
		Routing:      RoutingConfig{URL: "https://api.openai.com/v1/chat/completions"},
		Capabilities: EndpointCapabilities{SelfHosted: false, PrefixCacheEnabled: true},
		Extra:        json.RawMessage(`{"region":"us-east-1"}`),
	}
	auth, _ := EncodePayload(AuthTypeBearer, BearerAuth{APIKey: "sk-xxx"})
	ep.Auth = auth
	seedEndpoint(t, db, ep)

	r := NewSQLEndpointReader(db)
	got, err := r.PickForModel(context.Background(), "gpt-4o", "default")
	if err != nil {
		t.Fatalf("PickForModel: %v", err)
	}
	if got.Name != "openai_main" {
		t.Errorf("Name = %q", got.Name)
	}
	bearer, err := DecodePayload[BearerAuth](got.Auth)
	if err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if bearer.APIKey != "sk-xxx" {
		t.Errorf("APIKey = %q (encryption broken?)", bearer.APIKey)
	}
}

func TestSQLEndpointReader_PickEmptyGroupTreatedAsDefault(t *testing.T) {
	db := newTestDB(t)
	seedEndpoint(t, db, &Endpoint{Name: "x", Vendor: "openai", Model: "m"})

	r := NewSQLEndpointReader(db)
	got, err := r.PickForModel(context.Background(), "m", "")
	if err != nil {
		t.Fatalf("PickForModel: %v", err)
	}
	if got.Name != "x" {
		t.Errorf("got %q", got.Name)
	}
}

func TestSQLEndpointReader_PickNotFound(t *testing.T) {
	db := newTestDB(t)
	seedEndpoint(t, db, &Endpoint{Name: "x", Vendor: "openai", Model: "m", Group: "default"})

	r := NewSQLEndpointReader(db)
	if _, err := r.PickForModel(context.Background(), "missing", "default"); err == nil {
		t.Fatal("want not-found for missing model")
	}
	if _, err := r.PickForModel(context.Background(), "m", "reserved"); err == nil {
		t.Fatal("want not-found for missing group")
	}
}

func TestSQLEndpointReader_GetByID(t *testing.T) {
	db := newTestDB(t)
	ep := &Endpoint{Name: "ep1", Vendor: "openai", Model: "m"}
	seedEndpoint(t, db, ep)

	r := NewSQLEndpointReader(db)
	got, err := r.GetByID(context.Background(), ep.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Vendor != "openai" {
		t.Errorf("got %+v", got)
	}

	if _, err := r.GetByID(context.Background(), 99999); err == nil {
		t.Error("want not-found")
	}
}

func TestSQLEndpointReader_List(t *testing.T) {
	db := newTestDB(t)
	for _, name := range []string{"a", "b", "c"} {
		seedEndpoint(t, db, &Endpoint{Name: name, Vendor: "openai", Model: "m"})
	}

	r := NewSQLEndpointReader(db)
	all, _ := r.List(context.Background())
	if len(all) != 3 {
		t.Errorf("len = %d, want 3", len(all))
	}
}
