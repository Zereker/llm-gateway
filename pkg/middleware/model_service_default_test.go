package middleware

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zereker-labs/ai-gateway/pkg/config"
)

// minimal in-memory config.Store for tests.
type memStore struct {
	data map[string]json.RawMessage
}

func newMem(seed map[string]string) *memStore {
	m := &memStore{data: map[string]json.RawMessage{}}
	for k, v := range seed {
		m.data[k] = json.RawMessage(v)
	}
	return m
}

func (m *memStore) Get(_ context.Context, key string) (json.RawMessage, error) {
	return m.data[key], nil
}

func (m *memStore) List(_ context.Context, prefix string) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	for k, v := range m.data {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			out[k] = v
		}
	}
	return out, nil
}

func (m *memStore) Watch(_ context.Context, _ string) (<-chan config.Event, error) {
	return make(chan config.Event), nil
}

func (m *memStore) Put(_ context.Context, key string, value json.RawMessage) error {
	m.data[key] = value
	return nil
}

func (m *memStore) Delete(_ context.Context, key string) error {
	delete(m.data, key)
	return nil
}

func TestConfigBackedModelServiceProvider_GetByModel(t *testing.T) {
	store := newMem(map[string]string{
		"modelservice/svc_gpt4o": `{"ID":1,"ServiceID":"openai/gpt-4o","Model":"gpt-4o","Group":"default"}`,
		"modelservice/svc_claude": `{"ID":2,"ServiceID":"anthropic/claude","Model":"claude-3.5-sonnet"}`,
	})

	p, err := NewConfigBackedModelServiceProvider(context.Background(), store, "modelservice")
	if err != nil {
		t.Fatalf("NewConfigBackedModelServiceProvider: %v", err)
	}

	snap, err := p.GetByModel(context.Background(), "gpt-4o")
	if err != nil {
		t.Fatalf("GetByModel: %v", err)
	}
	if snap.ServiceID != "openai/gpt-4o" {
		t.Errorf("ServiceID = %q, want openai/gpt-4o", snap.ServiceID)
	}
}

func TestConfigBackedModelServiceProvider_NotFound(t *testing.T) {
	store := newMem(nil)
	p, _ := NewConfigBackedModelServiceProvider(context.Background(), store, "modelservice")

	if _, err := p.GetByModel(context.Background(), "missing"); err == nil {
		t.Fatal("want not found error")
	}
	if _, err := p.GetByModel(context.Background(), ""); err == nil {
		t.Fatal("want error for empty model")
	}
}

func TestConfigBackedModelServiceProvider_RejectsBadJSON(t *testing.T) {
	store := newMem(map[string]string{
		"modelservice/bad": `{not valid json}`,
	})
	_, err := NewConfigBackedModelServiceProvider(context.Background(), store, "modelservice")
	if err == nil {
		t.Fatal("want parse error")
	}
}

func TestConfigBackedModelServiceProvider_RejectsEmptyModelField(t *testing.T) {
	store := newMem(map[string]string{
		"modelservice/m1": `{"ID":1,"ServiceID":"x"}`, // no Model
	})
	_, err := NewConfigBackedModelServiceProvider(context.Background(), store, "modelservice")
	if err == nil {
		t.Fatal("want error for missing Model field")
	}
}

func TestConfigBackedModelServiceProvider_List(t *testing.T) {
	store := newMem(map[string]string{
		"modelservice/m1": `{"Model":"a"}`,
		"modelservice/m2": `{"Model":"b"}`,
	})
	p, _ := NewConfigBackedModelServiceProvider(context.Background(), store, "modelservice")

	all, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("got %d, want 2", len(all))
	}
}

func TestConfigBackedModelServiceProvider_Reload(t *testing.T) {
	store := newMem(map[string]string{
		"modelservice/m1": `{"Model":"a"}`,
	})
	p, _ := NewConfigBackedModelServiceProvider(context.Background(), store, "modelservice")

	// add another model and reload
	store.data["modelservice/m2"] = json.RawMessage(`{"Model":"b"}`)
	if err := p.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, err := p.GetByModel(context.Background(), "b"); err != nil {
		t.Errorf("GetByModel(b) after Reload: %v", err)
	}
}
