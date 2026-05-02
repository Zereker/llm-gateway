package repo

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

func TestSQLEndpointRepo_CreateAndPick(t *testing.T) {
	r := NewSQLEndpointRepo(newTestDB(t))
	ctx := context.Background()

	ep := &domain.Endpoint{
		ID:     "openai_main",
		Vendor: "openai",
		URL:    "https://api.openai.com",
		APIKey: domain.Secret("sk-xxx"),
		Group:  "default",
		Model:  "gpt-4o",
		Weight: 100,
		RPM:    600,
		TPM:    100000,
		Capabilities: domain.EndpointCapabilities{SelfHosted: false, PrefixCacheEnabled: true},
		Extra:        json.RawMessage(`{"region":"us-east-1"}`),
	}
	if err := r.Create(ctx, ep); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := r.PickForModel(ctx, "gpt-4o", "default")
	if err != nil {
		t.Fatalf("PickForModel: %v", err)
	}
	if got.ID != "openai_main" || string(got.APIKey) != "sk-xxx" {
		t.Errorf("got %+v", got)
	}
	if !got.Capabilities.PrefixCacheEnabled {
		t.Error("Capabilities not round-tripped")
	}
	if string(got.Extra) != `{"region":"us-east-1"}` {
		t.Errorf("Extra = %s", got.Extra)
	}
}

func TestSQLEndpointRepo_PickEmptyGroupTreatedAsDefault(t *testing.T) {
	r := NewSQLEndpointRepo(newTestDB(t))
	ctx := context.Background()

	_ = r.Create(ctx, &domain.Endpoint{ID: "x", Vendor: "v", URL: "u", Model: "m"})

	// 入参 group="" → 默认 "default"，且 schema DEFAULT 也是 "default"
	got, err := r.PickForModel(ctx, "m", "")
	if err != nil {
		t.Fatalf("PickForModel: %v", err)
	}
	if got.ID != "x" {
		t.Errorf("got %q", got.ID)
	}
}

func TestSQLEndpointRepo_PickNotFound(t *testing.T) {
	r := NewSQLEndpointRepo(newTestDB(t))
	ctx := context.Background()
	_ = r.Create(ctx, &domain.Endpoint{ID: "x", Vendor: "v", URL: "u", Model: "m", Group: "default"})

	if _, err := r.PickForModel(ctx, "missing", "default"); err == nil {
		t.Fatal("want not-found for missing model")
	}
	if _, err := r.PickForModel(ctx, "m", "reserved"); err == nil {
		t.Fatal("want not-found for missing group")
	}
}

func TestSQLEndpointRepo_GetByID(t *testing.T) {
	r := NewSQLEndpointRepo(newTestDB(t))
	ctx := context.Background()
	_ = r.Create(ctx, &domain.Endpoint{ID: "ep1", Vendor: "v", URL: "u", Model: "m"})

	got, err := r.GetByID(ctx, "ep1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Vendor != "v" {
		t.Errorf("got %+v", got)
	}

	if _, err := r.GetByID(ctx, "missing"); err == nil {
		t.Error("want not-found")
	}
}

func TestSQLEndpointRepo_List(t *testing.T) {
	r := NewSQLEndpointRepo(newTestDB(t))
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		_ = r.Create(ctx, &domain.Endpoint{ID: id, Vendor: "v", URL: "u", Model: "m"})
	}
	all, _ := r.List(ctx)
	if len(all) != 3 {
		t.Errorf("len = %d, want 3", len(all))
	}
}

func TestSQLEndpointRepo_Update(t *testing.T) {
	r := NewSQLEndpointRepo(newTestDB(t))
	ctx := context.Background()
	ep := &domain.Endpoint{ID: "ep1", Vendor: "v", URL: "u", Model: "m", Weight: 10}
	_ = r.Create(ctx, ep)

	ep.Weight = 200
	ep.URL = "https://new.example.com"
	if err := r.Update(ctx, ep); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := r.GetByID(ctx, "ep1")
	if got.Weight != 200 || got.URL != "https://new.example.com" {
		t.Errorf("got %+v", got)
	}
}

func TestSQLEndpointRepo_UpdateMissing(t *testing.T) {
	r := NewSQLEndpointRepo(newTestDB(t))
	err := r.Update(context.Background(), &domain.Endpoint{ID: "nope", Vendor: "v", Model: "m"})
	if err == nil {
		t.Fatal("want not-found")
	}
}

func TestSQLEndpointRepo_Delete(t *testing.T) {
	r := NewSQLEndpointRepo(newTestDB(t))
	ctx := context.Background()
	_ = r.Create(ctx, &domain.Endpoint{ID: "ep1", Vendor: "v", URL: "u", Model: "m"})

	if err := r.Delete(ctx, "ep1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.GetByID(ctx, "ep1"); err == nil {
		t.Error("expected gone after Delete")
	}
}

func TestSQLEndpointRepo_DeleteMissing(t *testing.T) {
	r := NewSQLEndpointRepo(newTestDB(t))
	if err := r.Delete(context.Background(), "nope"); err == nil {
		t.Fatal("want not-found")
	}
}

func TestSQLEndpointRepo_DuplicateID(t *testing.T) {
	r := NewSQLEndpointRepo(newTestDB(t))
	ctx := context.Background()
	_ = r.Create(ctx, &domain.Endpoint{ID: "x", Vendor: "v", URL: "u", Model: "m"})

	if err := r.Create(ctx, &domain.Endpoint{ID: "x", Vendor: "v2", URL: "u2", Model: "m"}); err == nil {
		t.Fatal("want PRIMARY KEY violation on duplicate id")
	}
}
