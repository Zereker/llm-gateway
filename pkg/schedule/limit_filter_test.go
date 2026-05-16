package schedule

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
	"github.com/zereker/llm-gateway/pkg/repo"
)

type localStubStore struct {
	allowedFor map[int64]bool
	err        error
	calls      atomic.Int32
}

func (s *localStubStore) ReserveBatch(_ context.Context, buckets []ratelimit.Bucket) (bool, *ratelimit.BucketViolation, error) {
	s.calls.Add(1)
	if s.err != nil {
		return false, nil, s.err
	}
	for _, b := range buckets {
		var id int64
		_, _ = scanID(b.Key, &id)
		if s.allowedFor != nil {
			if v, ok := s.allowedFor[id]; ok && !v {
				return false, &ratelimit.BucketViolation{Key: b.Key, Limit: b.Limit, Current: b.Limit + 1}, nil
			}
		}
	}
	return true, nil, nil
}
func (s *localStubStore) AdjustBatch(_ context.Context, _ []ratelimit.BucketAdjust) error {
	return nil
}
func (s *localStubStore) Snapshot(_ context.Context, _ ratelimit.Bucket) (ratelimit.BucketState, error) {
	return ratelimit.BucketState{}, nil
}

func scanID(key string, out *int64) (int, error) {
	parts := strings.Split(key, ":")
	if len(parts) < 3 {
		return 0, errors.New("bad key")
	}
	var n int64
	for _, c := range parts[2] {
		if c < '0' || c > '9' {
			return 0, errors.New("not digit")
		}
		n = n*10 + int64(c-'0')
	}
	*out = n
	return 1, nil
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
	cands := []*domain.Endpoint{ep(1, 100)}
	got := f.Apply(context.Background(), cands, &Request{TPMCost: 100})
	if len(got) != 1 {
		t.Errorf("got=%d, want 1", len(got))
	}
	if store.calls.Load() != 0 {
		t.Errorf("Store should not be called when ep has no quota; got %d", store.calls.Load())
	}
}

func TestLimitReadFilter_OverLimit_Excluded(t *testing.T) {
	store := &localStubStore{allowedFor: map[int64]bool{1: true, 2: false}}
	f := NewLimitReadFilter(store)
	cands := []*domain.Endpoint{epWithQuota(1, 60, 0), epWithQuota(2, 60, 0)}
	got := f.Apply(context.Background(), cands, &Request{TPMCost: 100})
	if len(got) != 1 || got[0].ID != 1 {
		t.Errorf("got=%+v, want [ep1]", got)
	}
}

func TestLimitReadFilter_FailOpen_OnStoreErr(t *testing.T) {
	store := &localStubStore{err: errors.New("redis down")}
	f := NewLimitReadFilter(store)
	cands := []*domain.Endpoint{epWithQuota(1, 60, 0), epWithQuota(2, 60, 0)}
	got := f.Apply(context.Background(), cands, &Request{TPMCost: 100})
	if len(got) != 2 {
		t.Errorf("fail-open expected, got %d", len(got))
	}
}

func TestEndpointTPMBucketKeys_NoTPM_Empty(t *testing.T) {
	e := ep(1, 100)
	if keys := EndpointTPMBucketKeys(e); len(keys) != 0 {
		t.Errorf("got=%+v", keys)
	}
}

func TestEndpointTPMBucketKeys_HasTPM(t *testing.T) {
	e := epWithQuota(42, 0, 100000)
	keys := EndpointTPMBucketKeys(e)
	if len(keys) != 1 || keys[0] != "rl:endpoint:42:tpm" {
		t.Errorf("got=%+v", keys)
	}
}

func TestBuildEndpointBuckets_RPMOnly(t *testing.T) {
	e := epWithQuota(7, 60, 0)
	bs := buildEndpointBuckets(e, 100)
	if len(bs) != 1 || !strings.HasSuffix(bs[0].Key, ":rpm") {
		t.Errorf("buckets=%+v", bs)
	}
	if bs[0].Cost != 1 {
		t.Errorf("rpm cost=%d", bs[0].Cost)
	}
}

func TestBuildEndpointBuckets_TPM_UsesPassedCost(t *testing.T) {
	e := epWithQuota(7, 0, 100000)
	bs := buildEndpointBuckets(e, 555)
	if bs[0].Cost != 555 {
		t.Errorf("tpm cost=%d", bs[0].Cost)
	}
}

func TestBuildEndpointBuckets_RPS(t *testing.T) {
	e := ep(7, 100)
	rps := uint32(10)
	e.Quota = repo.QuotaConfig{RPS: &rps}
	bs := buildEndpointBuckets(e, 100)
	if len(bs) != 1 || !strings.HasSuffix(bs[0].Key, ":rps") {
		t.Errorf("buckets=%+v", bs)
	}
}
