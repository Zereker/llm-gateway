package repo

import (
	"context"
	"testing"

	"github.com/jmoiron/sqlx"
	"gorm.io/datatypes"
)

const testTenant = "default"

// seedEndpoint 用 raw SQL 插测试 endpoint。tenant_id 默认 "default"。
func seedEndpoint(t *testing.T, db *sqlx.DB, ep *Endpoint) {
	t.Helper()
	if ep.TenantID == "" {
		ep.TenantID = testTenant
	}
	if ep.Group == "" {
		ep.Group = "default"
	}
	if ep.Weight == 0 {
		ep.Weight = 100
	}
	caps, _ := ep.Capabilities.Value()
	_, err := db.Exec(
		`INSERT INTO endpoints
		 (tenant_id, id, vendor, url, api_key, group_name, model, weight, rpm, tpm, rps, capabilities, extra)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ep.TenantID, ep.ID, ep.Vendor, ep.URL, string(ep.APIKey), ep.Group, ep.Model,
		ep.Weight, ep.RPM, ep.TPM, ep.RPS, caps, datatypesJSONOrNil(ep.Extra),
	)
	if err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}
}

func TestSQLEndpointReader_PickForModel(t *testing.T) {
	db := newTestDB(t)
	seedEndpoint(t, db, &Endpoint{
		ID:           "openai_main",
		Vendor:       "openai",
		URL:          "https://api.openai.com",
		APIKey:       Secret("sk-xxx"),
		Group:        "default",
		Model:        "gpt-4o",
		Weight:       100,
		RPM:          600,
		TPM:          100000,
		Capabilities: EndpointCapabilities{SelfHosted: false, PrefixCacheEnabled: true},
		Extra:        datatypes.JSON(`{"region":"us-east-1"}`),
	})

	r := NewSQLEndpointReader(db)
	got, err := r.PickForModel(context.Background(), testTenant, "gpt-4o", "default")
	if err != nil {
		t.Fatalf("PickForModel: %v", err)
	}
	if got.ID != "openai_main" || got.APIKey.Reveal() != "sk-xxx" {
		t.Errorf("got %+v", got)
	}
	if !got.Capabilities.PrefixCacheEnabled {
		t.Error("Capabilities not round-tripped")
	}
	if !jsonEqual(t, got.Extra, []byte(`{"region":"us-east-1"}`)) {
		t.Errorf("Extra = %s", got.Extra)
	}
}

func TestSQLEndpointReader_PickEmptyGroupTreatedAsDefault(t *testing.T) {
	db := newTestDB(t)
	seedEndpoint(t, db, &Endpoint{ID: "x", Vendor: "v", URL: "u", Model: "m"})

	r := NewSQLEndpointReader(db)
	got, err := r.PickForModel(context.Background(), testTenant, "m", "")
	if err != nil {
		t.Fatalf("PickForModel: %v", err)
	}
	if got.ID != "x" {
		t.Errorf("got %q", got.ID)
	}
}

func TestSQLEndpointReader_PickNotFound(t *testing.T) {
	db := newTestDB(t)
	seedEndpoint(t, db, &Endpoint{ID: "x", Vendor: "v", URL: "u", Model: "m", Group: "default"})

	r := NewSQLEndpointReader(db)
	if _, err := r.PickForModel(context.Background(), testTenant, "missing", "default"); err == nil {
		t.Fatal("want not-found for missing model")
	}
	if _, err := r.PickForModel(context.Background(), testTenant, "m", "reserved"); err == nil {
		t.Fatal("want not-found for missing group")
	}
}

func TestSQLEndpointReader_PickTenantIsolation(t *testing.T) {
	// 不同租户同 model 互不可见
	db := newTestDB(t)
	seedEndpoint(t, db, &Endpoint{TenantID: "tenant_a", ID: "ep_a", Vendor: "v", URL: "u", Model: "shared"})
	seedEndpoint(t, db, &Endpoint{TenantID: "tenant_b", ID: "ep_b", Vendor: "v", URL: "u", Model: "shared"})

	r := NewSQLEndpointReader(db)
	a, _ := r.PickForModel(context.Background(), "tenant_a", "shared", "default")
	b, _ := r.PickForModel(context.Background(), "tenant_b", "shared", "default")
	if a.ID != "ep_a" || b.ID != "ep_b" {
		t.Errorf("tenant isolation broken: a=%v b=%v", a, b)
	}
	if _, err := r.PickForModel(context.Background(), "tenant_c", "shared", "default"); err == nil {
		t.Error("tenant_c should see nothing")
	}
}

func TestSQLEndpointReader_GetByID(t *testing.T) {
	db := newTestDB(t)
	seedEndpoint(t, db, &Endpoint{ID: "ep1", Vendor: "v", URL: "u", Model: "m"})

	r := NewSQLEndpointReader(db)
	got, err := r.GetByID(context.Background(), testTenant, "ep1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Vendor != "v" {
		t.Errorf("got %+v", got)
	}

	if _, err := r.GetByID(context.Background(), testTenant, "missing"); err == nil {
		t.Error("want not-found")
	}
}

func TestSQLEndpointReader_List(t *testing.T) {
	db := newTestDB(t)
	for _, id := range []string{"a", "b", "c"} {
		seedEndpoint(t, db, &Endpoint{ID: id, Vendor: "v", URL: "u", Model: "m"})
	}

	r := NewSQLEndpointReader(db)
	all, _ := r.List(context.Background(), testTenant)
	if len(all) != 3 {
		t.Errorf("len = %d, want 3", len(all))
	}
}
