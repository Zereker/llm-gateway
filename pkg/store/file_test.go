package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestFileKV_PutGet(t *testing.T) {
	s, _ := NewFileKV(t.TempDir())
	ctx := context.Background()

	if err := s.Put(ctx, "ratelimit/user/alice/svc_gpt4o", json.RawMessage(`{"RPM":60}`)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(ctx, "ratelimit/user/alice/svc_gpt4o")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != `{"RPM":60}` {
		t.Errorf("Get = %s, want %s", got, `{"RPM":60}`)
	}
}

func TestFileKV_GetMissing(t *testing.T) {
	s, _ := NewFileKV(t.TempDir())
	if _, err := s.Get(context.Background(), "no-such"); err == nil {
		t.Fatal("want error for missing key")
	}
}

func TestFileKV_List(t *testing.T) {
	s, _ := NewFileKV(t.TempDir())
	ctx := context.Background()

	_ = s.Put(ctx, "modelservice/gpt-4o", json.RawMessage(`{"Model":"gpt-4o"}`))
	_ = s.Put(ctx, "modelservice/claude", json.RawMessage(`{"Model":"claude-3.5"}`))
	_ = s.Put(ctx, "ratelimit/whatever", json.RawMessage(`{}`))

	got, err := s.List(ctx, "modelservice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("List returned %d, want 2 entries; got: %v", len(got), got)
	}
	if _, ok := got["modelservice/gpt-4o"]; !ok {
		t.Errorf("missing modelservice/gpt-4o")
	}
}

func TestFileKV_ListMissingPrefix(t *testing.T) {
	s, _ := NewFileKV(t.TempDir())
	got, err := s.List(context.Background(), "no-such-prefix")
	if err != nil {
		t.Fatalf("List should not error on missing prefix: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want empty map, got %v", got)
	}
}

func TestFileKV_Delete(t *testing.T) {
	s, _ := NewFileKV(t.TempDir())
	ctx := context.Background()

	_ = s.Put(ctx, "k", json.RawMessage(`"v"`))
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "k"); err == nil {
		t.Fatal("Get after Delete should fail")
	}
	// idempotent
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete again should be idempotent: %v", err)
	}
}

func TestFileKV_WatchClosesOnCtx(t *testing.T) {
	s, _ := NewFileKV(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())

	ch, err := s.Watch(ctx, "modelservice")
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("want closed channel after ctx cancel; got value")
	}
}

func TestFileKV_EmptyRootRejected(t *testing.T) {
	if _, err := NewFileKV(""); err == nil {
		t.Fatal("want error for empty root")
	}
}

func TestFileKV_PathOf(t *testing.T) {
	s, _ := NewFileKV("/tmp/test")
	want := filepath.Join("/tmp/test", "a/b/c.json")
	if got := s.pathOf("a/b/c"); got != want {
		t.Errorf("pathOf = %q, want %q", got, want)
	}
}
