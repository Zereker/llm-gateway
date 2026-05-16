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

// =============================================================================
// stub: ratelimit.Store
// =============================================================================

// localStubStore 按 ep.ID 决定 ReserveBatch 的返回（key 形如 rl:endpoint:<id>:rpm）。
//
// allowedFor[id]=false 时该 endpoint 的 reserve 被拒；nil map 默认全过。
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
		// 从 key 抠 ep id
		var id int64
		_, _ = fmtScan(b.Key, &id)
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

// fmtScan 简单从 "rl:endpoint:<id>:..." 抠 int64 id；测试用。
func fmtScan(key string, out *int64) (int, error) {
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

// epWithQuota 构造带 quota 的 endpoint
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

// =============================================================================
// LimitReadFilter tests
// =============================================================================

func TestLimitReadFilter_Empty_Passthrough(t *testing.T) {
	f := NewLimitReadFilter(&localStubStore{})
	got := f.Apply(context.Background(), nil, &Request{})
	if got != nil {
		t.Errorf("got=%+v, want nil", got)
	}
}

func TestLimitReadFilter_NilStore_Passthrough(t *testing.T) {
	f := NewLimitReadFilter(nil)
	cands := []*domain.Endpoint{ep(1, 100)}
	got := f.Apply(context.Background(), cands, &Request{})
	if len(got) != 1 {
		t.Errorf("nil store should passthrough, got %+v", got)
	}
}

func TestLimitReadFilter_NoQuotaConfig_Passthrough(t *testing.T) {
	// ep 没配 quota → 直接保留，不调 Store
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
		t.Errorf("got=%+v, want [ep1] (ep2 over-limit)", got)
	}
}

func TestLimitReadFilter_FailOpen_OnStoreErr(t *testing.T) {
	store := &localStubStore{err: errors.New("redis down")}
	f := NewLimitReadFilter(store)
	cands := []*domain.Endpoint{epWithQuota(1, 60, 0), epWithQuota(2, 60, 0)}
	got := f.Apply(context.Background(), cands, &Request{TPMCost: 100})

	if len(got) != 2 {
		t.Errorf("fail-open on err should preserve all, got %d", len(got))
	}
}

func TestLimitReadFilter_Name(t *testing.T) {
	f := NewLimitReadFilter(nil)
	if f.Name() != "limit_read" {
		t.Errorf("name=%q", f.Name())
	}
}

// =============================================================================
// EndpointTPMBucketKeys / buildEndpointBuckets
// =============================================================================

func TestEndpointTPMBucketKeys_NoTPM_Empty(t *testing.T) {
	e := ep(1, 100)
	keys := EndpointTPMBucketKeys(e)
	if len(keys) != 0 {
		t.Errorf("got=%+v, want []", keys)
	}
}

func TestEndpointTPMBucketKeys_HasTPM(t *testing.T) {
	e := epWithQuota(42, 0, 100000)
	keys := EndpointTPMBucketKeys(e)
	if len(keys) != 1 || keys[0] != "rl:endpoint:42:tpm" {
		t.Errorf("got=%+v, want [rl:endpoint:42:tpm]", keys)
	}
}

func TestBuildEndpointBuckets_RPMOnly(t *testing.T) {
	e := epWithQuota(7, 60, 0)
	buckets := buildEndpointBuckets(e, 100)
	if len(buckets) != 1 || !strings.HasSuffix(buckets[0].Key, ":rpm") {
		t.Errorf("buckets=%+v", buckets)
	}
	if buckets[0].Cost != 1 {
		t.Errorf("rpm cost=%d, want 1", buckets[0].Cost)
	}
}

func TestBuildEndpointBuckets_TPM_UsesPassedCost(t *testing.T) {
	e := epWithQuota(7, 0, 100000)
	buckets := buildEndpointBuckets(e, 555)
	if buckets[0].Cost != 555 {
		t.Errorf("tpm cost=%d, want 555", buckets[0].Cost)
	}
}

func TestBuildEndpointBuckets_RPS(t *testing.T) {
	e := ep(7, 100)
	rps := uint32(10)
	e.Quota = repo.QuotaConfig{RPS: &rps}
	buckets := buildEndpointBuckets(e, 100)
	if len(buckets) != 1 || !strings.HasSuffix(buckets[0].Key, ":rps") {
		t.Errorf("buckets=%+v", buckets)
	}
}
