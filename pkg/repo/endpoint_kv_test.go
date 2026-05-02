package repo

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestKVEndpointProvider_PickFirstMatchingGroup(t *testing.T) {
	kv := newMem(map[string]string{
		"endpoint/openai_main":     `{"ID":"openai_main","Vendor":"openai","URL":"https://api.openai.com","Model":"gpt-4o","Group":"default"}`,
		"endpoint/openai_reserved": `{"ID":"openai_reserved","Vendor":"openai","URL":"https://api.openai.com","Model":"gpt-4o","Group":"reserved"}`,
	})

	p, err := NewKVEndpointProvider(context.Background(), kv, "endpoint")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ep, err := p.PickForModel(context.Background(), "gpt-4o", "default")
	if err != nil {
		t.Fatalf("PickForModel: %v", err)
	}
	if ep.ID != "openai_main" {
		t.Errorf("got %q, want openai_main", ep.ID)
	}

	ep, err = p.PickForModel(context.Background(), "gpt-4o", "reserved")
	if err != nil {
		t.Fatalf("PickForModel reserved: %v", err)
	}
	if ep.ID != "openai_reserved" {
		t.Errorf("got %q, want openai_reserved", ep.ID)
	}
}

func TestKVEndpointProvider_EmptyGroupTreatedAsDefault(t *testing.T) {
	kv := newMem(map[string]string{
		"endpoint/x": `{"ID":"x","Vendor":"openai","URL":"u","Model":"gpt-4o","Group":""}`,
	})
	p, _ := NewKVEndpointProvider(context.Background(), kv, "endpoint")

	// Empty Group field on Endpoint AND empty group arg → treated as default.
	ep, err := p.PickForModel(context.Background(), "gpt-4o", "")
	if err != nil {
		t.Fatalf("PickForModel: %v", err)
	}
	if ep.ID != "x" {
		t.Errorf("got %q, want x", ep.ID)
	}
}

func TestKVEndpointProvider_ModelNotFound(t *testing.T) {
	kv := newMem(map[string]string{
		"endpoint/x": `{"ID":"x","Vendor":"openai","URL":"u","Model":"gpt-4o","Group":"default"}`,
	})
	p, _ := NewKVEndpointProvider(context.Background(), kv, "endpoint")

	_, err := p.PickForModel(context.Background(), "claude-3.5", "default")
	if err == nil {
		t.Fatal("want error for missing model")
	}
}

func TestKVEndpointProvider_GroupNotFound(t *testing.T) {
	kv := newMem(map[string]string{
		"endpoint/x": `{"ID":"x","Vendor":"openai","URL":"u","Model":"gpt-4o","Group":"default"}`,
	})
	p, _ := NewKVEndpointProvider(context.Background(), kv, "endpoint")

	_, err := p.PickForModel(context.Background(), "gpt-4o", "reserved")
	if err == nil {
		t.Fatal("want error for missing group")
	}
}

func TestKVEndpointProvider_RejectsEmptyModel(t *testing.T) {
	p, _ := NewKVEndpointProvider(context.Background(), newMem(nil), "endpoint")
	if _, err := p.PickForModel(context.Background(), "", "default"); err == nil {
		t.Fatal("want error for empty model")
	}
}

func TestKVEndpointProvider_RejectsBadConfig(t *testing.T) {
	cases := map[string]string{
		"bad json":       `{not json}`,
		"missing ID":     `{"Vendor":"openai","Model":"x"}`,
		"missing Model":  `{"ID":"x","Vendor":"openai"}`,
		"missing Vendor": `{"ID":"x","Model":"x"}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			kv := newMem(map[string]string{"endpoint/bad": raw})
			_, err := NewKVEndpointProvider(context.Background(), kv, "endpoint")
			if err == nil {
				t.Fatalf("want error for %s", name)
			}
		})
	}
}

func TestKVEndpointProvider_List(t *testing.T) {
	kv := newMem(map[string]string{
		"endpoint/a": `{"ID":"a","Vendor":"v","Model":"m","Group":"default"}`,
		"endpoint/b": `{"ID":"b","Vendor":"v","Model":"m","Group":"default"}`,
	})
	p, _ := NewKVEndpointProvider(context.Background(), kv, "endpoint")

	all, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("got %d, want 2", len(all))
	}
}

func TestKVEndpointProvider_Reload(t *testing.T) {
	kv := newMem(map[string]string{
		"endpoint/a": `{"ID":"a","Vendor":"v","Model":"m1","Group":"default"}`,
	})
	p, _ := NewKVEndpointProvider(context.Background(), kv, "endpoint")

	if _, err := p.PickForModel(context.Background(), "m2", "default"); err == nil {
		t.Fatal("want error before reload")
	}

	kv.data["endpoint/b"] = json.RawMessage(`{"ID":"b","Vendor":"v","Model":"m2","Group":"default"}`)
	if err := p.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, err := p.PickForModel(context.Background(), "m2", "default"); err != nil {
		t.Errorf("after Reload: %v", err)
	}
	_ = strings.Builder{} // prevent unused import
}
