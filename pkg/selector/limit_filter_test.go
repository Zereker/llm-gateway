package selector

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
)

// localStubStore SnapshotBatch-only stub；reserve / charge 全部不实现（不会被调）。
type localStubStore struct {
	// 按 ep ID 决定 SnapshotBatch 返回的 Used；超出 Limit 触发剔除
	usedFor map[int64]uint32
	err     error
	calls   atomic.Int32
}

func (s *localStubStore) SnapshotBatch(_ context.Context, buckets []ratelimit.Bucket) ([]ratelimit.BucketState, error) {
	s.calls.Add(1)
	if s.err != nil {
		return nil, s.err
	}
	out := make([]ratelimit.BucketState, len(buckets))
	for i, b := range buckets {
		var id int64
		_, _ = scanID(b.Key, &id)
		used := uint32(0)
		if s.usedFor != nil {
			used = s.usedFor[id]
		}
		out[i] = ratelimit.BucketState{Key: b.Key, Used: used, Limit: b.Limit}
	}
	return out, nil
}

func (s *localStubStore) ReserveBatch(_ context.Context, _ []ratelimit.Bucket) (bool, *ratelimit.BucketViolation, error) {
	return true, nil, nil
}
func (s *localStubStore) ChargeBatch(_ context.Context, _ []ratelimit.Bucket) ([]ratelimit.BucketChargeResult, error) {
	return nil, nil
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
	cands := []*domain.Endpoint{ep(1, 100)} // 无 quota
	got := f.Apply(context.Background(), cands, &Request{})
	if len(got) != 1 {
		t.Errorf("got=%d, want 1", len(got))
	}
	if store.calls.Load() != 0 {
		t.Errorf("Store should not be called when ep has no quota; got %d", store.calls.Load())
	}
}

func TestLimitReadFilter_OverLimit_Excluded(t *testing.T) {
	// ep2 已用满 60/60 → 应剔除
	store := &localStubStore{usedFor: map[int64]uint32{1: 0, 2: 60}}
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

func TestEndpointReserveBuckets_RPMOnly(t *testing.T) {
	e := epWithQuota(7, 60, 0)
	bs := EndpointReserveBuckets(e)
	if len(bs) != 1 || !strings.HasSuffix(bs[0].Key, ":rpm") {
		t.Errorf("buckets=%+v", bs)
	}
	if bs[0].Cost != 1 {
		t.Errorf("rpm cost=%d, want 1", bs[0].Cost)
	}
}

func TestEndpointReserveBuckets_NoTPMInReserve(t *testing.T) {
	// docs/04 §10：endpoint TPM 不在 reserve；只在 ChargeBatch 出现
	e := epWithQuota(7, 0, 100000)
	bs := EndpointReserveBuckets(e)
	if len(bs) != 0 {
		t.Errorf("TPM should not appear in reserve buckets, got=%+v", bs)
	}
}

func TestEndpointReserveBuckets_RPS(t *testing.T) {
	e := ep(7, 100)
	rps := uint32(10)
	e.Quota = domain.QuotaConfig{RPS: &rps}
	bs := EndpointReserveBuckets(e)
	if len(bs) != 1 || !strings.HasSuffix(bs[0].Key, ":rps") {
		t.Errorf("buckets=%+v", bs)
	}
}

func TestEndpointTPMChargeBucket_NoTPM_Nil(t *testing.T) {
	e := ep(1, 100)
	if got := EndpointTPMChargeBucket(e, 100); got != nil {
		t.Errorf("got=%+v, want nil", got)
	}
}

func TestEndpointTPMChargeBucket_HasTPM(t *testing.T) {
	e := epWithQuota(42, 0, 100000)
	got := EndpointTPMChargeBucket(e, 555)
	if got == nil || got.Key != "rl:endpoint:42:tpm" {
		t.Errorf("got=%+v", got)
	}
	if got.Cost != 555 {
		t.Errorf("cost=%d, want 555", got.Cost)
	}
}

func TestEndpointTPMChargeBucket_ZeroCost_Nil(t *testing.T) {
	e := epWithQuota(42, 0, 100000)
	if got := EndpointTPMChargeBucket(e, 0); got != nil {
		t.Errorf("cost=0 should return nil, got %+v", got)
	}
}
