package selector

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// localStubStore is a SnapshotBatch-only stub; reserve / charge are all unimplemented (never called).
type localStubStore struct {
	// decides the Used value SnapshotBatch returns, keyed by ep ID; exceeding Limit triggers exclusion
	available map[int64]bool
	err       error
	calls     atomic.Int32
}

func (s *localStubStore) Available(_ context.Context, endpoints []*domain.Endpoint) (map[int64]bool, error) {
	s.calls.Add(1)
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[int64]bool, len(endpoints))
	for _, endpoint := range endpoints {
		out[endpoint.ID] = true
		if value, ok := s.available[endpoint.ID]; ok {
			out[endpoint.ID] = value
		}
	}
	return out, nil
}

func epWithQuota(id int64, rpm, tpm uint32) *domain.Endpoint {
	e := ep(id, 100)
	if rpm > 0 {
		e.Quota.RPM = &rpm
	}
	if tpm > 0 {
		e.Quota.TPM = &tpm
	}
	return e
}

func TestLimitReadFilter_Empty_Passthrough(t *testing.T) {
	f := NewLimitReadFilter(&localStubStore{})
	got := f.Apply(context.Background(), nil, &Request{})
	if got != nil {
		t.Errorf("got=%+v", got)
	}
}

func TestLimitReadFilter_NilStore_Passthrough(t *testing.T) {
	f := NewLimitReadFilter(nil)
	cands := []*domain.Endpoint{ep(1, 100)}
	got := f.Apply(context.Background(), cands, &Request{})
	if len(got) != 1 {
		t.Errorf("nil store passthrough fail: %+v", got)
	}
}

func TestLimitReadFilter_NoQuotaConfig_Passthrough(t *testing.T) {
	store := &localStubStore{}
	f := NewLimitReadFilter(store)
	cands := []*domain.Endpoint{ep(1, 100)} // no quota
	got := f.Apply(context.Background(), cands, &Request{})
	if len(got) != 1 {
		t.Errorf("got=%d, want 1", len(got))
	}
	if store.calls.Load() != 0 {
		t.Errorf("Store should not be called when ep has no quota; got %d", store.calls.Load())
	}
}

func TestLimitReadFilter_OverLimit_Excluded(t *testing.T) {
	// ep2 already used up 60/60 → should be excluded
	store := &localStubStore{available: map[int64]bool{1: true, 2: false}}
	f := NewLimitReadFilter(store)
	cands := []*domain.Endpoint{epWithQuota(1, 60, 0), epWithQuota(2, 60, 0)}
	got := f.Apply(context.Background(), cands, &Request{})
	if len(got) != 1 || got[0].ID != 1 {
		t.Errorf("got=%+v, want [ep1]", got)
	}
}

func TestLimitReadFilter_FailOpen_OnStoreErr(t *testing.T) {
	store := &localStubStore{err: errors.New("redis down")}
	f := NewLimitReadFilter(store)
	cands := []*domain.Endpoint{epWithQuota(1, 60, 0), epWithQuota(2, 60, 0)}
	got := f.Apply(context.Background(), cands, &Request{})
	if len(got) != 2 {
		t.Errorf("fail-open expected, got %d", len(got))
	}
}
