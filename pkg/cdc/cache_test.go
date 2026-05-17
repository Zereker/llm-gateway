package cdc

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

type fakeMS struct {
	Model string `json:"Model"`
	ID    int64  `json:"ID"`
}

func TestTieredCache_L1Hit_DoesNotCallLoader(t *testing.T) {
	var loaderCalls atomic.Int32
	c := NewTieredCache[*fakeMS](
		TieredConfig{Table: "model_services"},
		NewLRU[*fakeMS](16),
		func(m *fakeMS) string {
			if m == nil {
				return ""
			}
			return m.Model
		},
		func(ctx context.Context, pk string) (*fakeMS, error) {
			loaderCalls.Add(1)
			return &fakeMS{Model: pk, ID: 1}, nil
		},
	)
	// 第一次：触发 loader → 写 L1
	v, err := c.Get(context.Background(), "gpt-4o")
	if err != nil || v.Model != "gpt-4o" {
		t.Fatalf("first get: %+v %v", v, err)
	}
	if loaderCalls.Load() != 1 {
		t.Errorf("loader should be called once, got %d", loaderCalls.Load())
	}
	// 第二次：L1 命中，loader 不应再调
	_, _ = c.Get(context.Background(), "gpt-4o")
	if loaderCalls.Load() != 1 {
		t.Errorf("loader should still be 1, got %d (L1 miss?)", loaderCalls.Load())
	}
}

func TestTieredCache_HandleEvent_InvalidatesL1(t *testing.T) {
	var loaderCalls atomic.Int32
	c := NewTieredCache[*fakeMS](
		TieredConfig{Table: "model_services"},
		NewLRU[*fakeMS](16),
		func(m *fakeMS) string {
			if m == nil {
				return ""
			}
			return m.Model
		},
		func(ctx context.Context, pk string) (*fakeMS, error) {
			loaderCalls.Add(1)
			return &fakeMS{Model: pk, ID: 42}, nil
		},
	)
	_, _ = c.Get(context.Background(), "gpt-4o")
	if loaderCalls.Load() != 1 {
		t.Fatal("L1 miss on first get?")
	}

	// CDC update event → L1 应该被失效
	ev := &Event{
		Op:    OpUpdate,
		After: []byte(`{"Model":"gpt-4o","ID":42}`),
	}
	if err := c.HandleEvent(context.Background(), "model_services", ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	// 再 Get 应当再次调 loader（L1 已失效）
	_, _ = c.Get(context.Background(), "gpt-4o")
	if loaderCalls.Load() != 2 {
		t.Errorf("expected loader=2 after invalidate, got %d", loaderCalls.Load())
	}
}

func TestTieredCache_HandleEvent_WrongTable_NoOp(t *testing.T) {
	c := NewTieredCache[*fakeMS](
		TieredConfig{Table: "model_services"},
		NewLRU[*fakeMS](16),
		func(m *fakeMS) string { return m.Model },
		nil,
	)
	c.local.Set("gpt-4o", &fakeMS{Model: "gpt-4o"})

	ev := &Event{Op: OpUpdate, After: []byte(`{"Model":"gpt-4o"}`)}
	_ = c.HandleEvent(context.Background(), "endpoints", ev) // 不是本 cache 的表
	if _, ok := c.local.Get("gpt-4o"); !ok {
		t.Error("wrong-table event should not invalidate this cache")
	}
}

func TestTieredCache_HandleEvent_Delete(t *testing.T) {
	c := NewTieredCache[*fakeMS](
		TieredConfig{Table: "model_services"},
		NewLRU[*fakeMS](16),
		func(m *fakeMS) string { return m.Model },
		nil,
	)
	c.local.Set("gpt-4o", &fakeMS{Model: "gpt-4o"})

	// delete event：before 有数据，after=null
	ev := &Event{Op: OpDelete, Before: []byte(`{"Model":"gpt-4o","ID":1}`)}
	_ = c.HandleEvent(context.Background(), "model_services", ev)
	if _, ok := c.local.Get("gpt-4o"); ok {
		t.Error("delete event should invalidate L1")
	}
}

func TestTieredCache_LoaderError_ReturnsErr(t *testing.T) {
	c := NewTieredCache[*fakeMS](
		TieredConfig{Table: "x"},
		NewLRU[*fakeMS](16),
		func(m *fakeMS) string { return "" },
		func(ctx context.Context, pk string) (*fakeMS, error) { return nil, errors.New("db down") },
	)
	_, err := c.Get(context.Background(), "anything")
	if err == nil {
		t.Fatal("expected loader err to propagate")
	}
}

func TestTieredCache_NotFound_NoCache(t *testing.T) {
	var calls atomic.Int32
	c := NewTieredCache[*fakeMS](
		TieredConfig{Table: "x"},
		NewLRU[*fakeMS](16),
		func(m *fakeMS) string { return m.Model },
		func(ctx context.Context, pk string) (*fakeMS, error) {
			calls.Add(1)
			return nil, nil // not found
		},
	)
	for i := 0; i < 3; i++ {
		_, _ = c.Get(context.Background(), "missing")
	}
	// not-found 不写 L1；每次都重新查 loader
	if calls.Load() != 3 {
		t.Errorf("not-found should not be cached; loader calls=%d want=3", calls.Load())
	}
}

func TestLRU_Cap_EvictsOldest(t *testing.T) {
	c := NewLRU[int](3)
	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("c", 3)
	c.Set("d", 4) // 应淘汰 a
	if _, ok := c.Get("a"); ok {
		t.Error("a should be evicted")
	}
	if v, _ := c.Get("d"); v != 4 {
		t.Errorf("d=%d", v)
	}
}

func TestLRU_Clear(t *testing.T) {
	c := NewLRU[int](3)
	c.Set("a", 1)
	c.Set("b", 2)
	c.Clear()
	if _, ok := c.Get("a"); ok {
		t.Error("clear should remove all")
	}
}
