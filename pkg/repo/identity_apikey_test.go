package repo

import (
	"context"
	"testing"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

func TestAPIKeyProvider_ResolveKnown(t *testing.T) {
	want := domain.UserIdentity{UserID: "alice", Group: "default"}
	p := NewAPIKeyProvider(map[string]domain.UserIdentity{
		"sk-aaa": want,
	})

	got, err := p.Resolve(context.Background(), &Credentials{APIKey: "sk-aaa"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.UserID != want.UserID || got.Group != want.Group {
		t.Errorf("got %+v, want %+v", *got, want)
	}
}

func TestAPIKeyProvider_ResolveUnknown(t *testing.T) {
	p := NewAPIKeyProvider(nil)
	_, err := p.Resolve(context.Background(), &Credentials{APIKey: "no-such"})
	if err == nil {
		t.Fatal("want error for unknown key")
	}
}

func TestAPIKeyProvider_ResolveNilCreds(t *testing.T) {
	p := NewAPIKeyProvider(nil)
	if _, err := p.Resolve(context.Background(), nil); err == nil {
		t.Fatal("want error for nil creds")
	}
	if _, err := p.Resolve(context.Background(), &Credentials{APIKey: ""}); err == nil {
		t.Fatal("want error for empty api key")
	}
}

func TestAPIKeyProvider_UpdateReplacesTable(t *testing.T) {
	p := NewAPIKeyProvider(map[string]domain.UserIdentity{
		"sk-old": {UserID: "alice"},
	})

	p.Update(map[string]domain.UserIdentity{
		"sk-new": {UserID: "bob"},
	})

	if _, err := p.Resolve(context.Background(), &Credentials{APIKey: "sk-old"}); err == nil {
		t.Error("old key should be gone after Update")
	}
	id, err := p.Resolve(context.Background(), &Credentials{APIKey: "sk-new"})
	if err != nil {
		t.Fatalf("new key not found after Update: %v", err)
	}
	if id.UserID != "bob" {
		t.Errorf("got UserID %q, want bob", id.UserID)
	}
}

func TestAPIKeyProvider_InputMapNotShared(t *testing.T) {
	src := map[string]domain.UserIdentity{"sk": {UserID: "alice"}}
	p := NewAPIKeyProvider(src)

	src["sk-injected"] = domain.UserIdentity{UserID: "mallory"}

	if _, err := p.Resolve(context.Background(), &Credentials{APIKey: "sk-injected"}); err == nil {
		t.Fatal("provider should not share input map")
	}
}
