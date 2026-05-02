package repo

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zereker-labs/ai-gateway/pkg/store"
)

// minimal in-memory store.KV for tests; shared by modelservice_kv_test and endpoint_kv_test.
type memKV struct {
	data map[string]json.RawMessage
}

func newMem(seed map[string]string) *memKV {
	m := &memKV{data: map[string]json.RawMessage{}}
	for k, v := range seed {
		m.data[k] = json.RawMessage(v)
	}
	return m
}

func (m *memKV) Get(_ context.Context, key string) (json.RawMessage, error) {
	return m.data[key], nil
}

func (m *memKV) List(_ context.Context, prefix string) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	for k, v := range m.data {
		if strings.HasPrefix(k, prefix) {
			out[k] = v
		}
	}
	return out, nil
}

func (m *memKV) Watch(_ context.Context, _ string) (<-chan store.Event, error) {
	return make(chan store.Event), nil
}

func (m *memKV) Put(_ context.Context, key string, value json.RawMessage) error {
	m.data[key] = value
	return nil
}

func (m *memKV) Delete(_ context.Context, key string) error {
	delete(m.data, key)
	return nil
}

func TestKVModelServiceProvider_GetByModel(t *testing.T) {
	kv := newMem(map[string]string{
		"modelservice/svc_gpt4o":  `{"ID":1,"ServiceID":"openai/gpt-4o","Model":"gpt-4o","Group":"default"}`,
		"modelservice/svc_claude": `{"ID":2,"ServiceID":"anthropic/claude","Model":"claude-3.5-sonnet"}`,
	})

	p, err := NewKVModelServiceProvider(context.Background(), kv, "modelservice")
	if err != nil {
		t.Fatalf("NewKVModelServiceProvider: %v", err)
	}

	snap, err := p.GetByModel(context.Background(), "gpt-4o")
	if err != nil {
		t.Fatalf("GetByModel: %v", err)
	}
	if snap.ServiceID != "openai/gpt-4o" {
		t.Errorf("ServiceID = %q, want openai/gpt-4o", snap.ServiceID)
	}
}

func TestKVModelServiceProvider_NotFound(t *testing.T) {
	kv := newMem(nil)
	p, _ := NewKVModelServiceProvider(context.Background(), kv, "modelservice")

	if _, err := p.GetByModel(context.Background(), "missing"); err == nil {
		t.Fatal("want not found error")
	}
	if _, err := p.GetByModel(context.Background(), ""); err == nil {
		t.Fatal("want error for empty model")
	}
}

func TestKVModelServiceProvider_RejectsBadJSON(t *testing.T) {
	kv := newMem(map[string]string{
		"modelservice/bad": `{not valid json}`,
	})
	_, err := NewKVModelServiceProvider(context.Background(), kv, "modelservice")
	if err == nil {
		t.Fatal("want parse error")
	}
}

func TestKVModelServiceProvider_RejectsEmptyModelField(t *testing.T) {
	kv := newMem(map[string]string{
		"modelservice/m1": `{"ID":1,"ServiceID":"x"}`, // no Model
	})
	_, err := NewKVModelServiceProvider(context.Background(), kv, "modelservice")
	if err == nil {
		t.Fatal("want error for missing Model field")
	}
}

func TestKVModelServiceProvider_List(t *testing.T) {
	kv := newMem(map[string]string{
		"modelservice/m1": `{"Model":"a"}`,
		"modelservice/m2": `{"Model":"b"}`,
	})
	p, _ := NewKVModelServiceProvider(context.Background(), kv, "modelservice")

	all, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("got %d, want 2", len(all))
	}
}

func TestKVModelServiceProvider_Reload(t *testing.T) {
	kv := newMem(map[string]string{
		"modelservice/m1": `{"Model":"a"}`,
	})
	p, _ := NewKVModelServiceProvider(context.Background(), kv, "modelservice")

	// add another model and reload
	kv.data["modelservice/m2"] = json.RawMessage(`{"Model":"b"}`)
	if err := p.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, err := p.GetByModel(context.Background(), "b"); err != nil {
		t.Errorf("GetByModel(b) after Reload: %v", err)
	}
}
