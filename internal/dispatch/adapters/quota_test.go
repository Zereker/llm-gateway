package adapters

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zereker/llm-gateway/internal/dispatch"
	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/ratelimit"
)

// recStore records ReleaseBatch calls and allows reserves.
type recStore struct {
	reserveAllowed  bool
	reserveViolated *ratelimit.BucketViolation
	reserveErr      error
	reserved        [][]ratelimit.Bucket
	charged         [][]ratelimit.Bucket
	released        [][]ratelimit.Bucket
	releaseErr      error
}

func (s *recStore) ReserveBatch(_ context.Context, buckets []ratelimit.Bucket) (bool, *ratelimit.BucketViolation, error) {
	s.reserved = append(s.reserved, buckets)
	return s.reserveAllowed, s.reserveViolated, s.reserveErr
}
func (s *recStore) ChargeBatch(_ context.Context, buckets []ratelimit.Bucket) ([]ratelimit.BucketChargeResult, error) {
	s.charged = append(s.charged, buckets)
	return nil, nil
}
func (s *recStore) SnapshotBatch(context.Context, []ratelimit.Bucket) ([]ratelimit.BucketState, error) {
	return nil, nil
}
func (s *recStore) ReleaseBatch(_ context.Context, b []ratelimit.Bucket) error {
	s.released = append(s.released, b)
	return s.releaseErr
}

func uint32ptr(value uint32) *uint32 { return &value }

func TestEndpointQuotaAdapterReserve(t *testing.T) {
	t.Run("allowed", func(t *testing.T) {
		store := &recStore{reserveAllowed: true}
		verdict, err := NewEndpointQuota(store).Reserve(context.Background(), epWithRPM(7, 100))
		if err != nil || verdict != nil || len(store.reserved) != 1 || len(store.reserved[0]) != 1 {
			t.Fatalf("verdict=%+v err=%v reserved=%v", verdict, err, store.reserved)
		}
	})

	t.Run("capacity with violation", func(t *testing.T) {
		store := &recStore{reserveViolated: &ratelimit.BucketViolation{Key: "rl:endpoint:7:rpm"}}
		verdict, err := NewEndpointQuota(store).Reserve(context.Background(), epWithRPM(7, 100))
		if err != nil || verdict == nil || verdict.Class != dispatch.ClassCapacity || verdict.BucketKey != "rl:endpoint:7:rpm" {
			t.Fatalf("verdict=%+v err=%v", verdict, err)
		}
	})

	t.Run("capacity without violation detail", func(t *testing.T) {
		verdict, err := NewEndpointQuota(&recStore{}).Reserve(context.Background(), epWithRPM(7, 100))
		if err != nil || verdict == nil || verdict.Class != dispatch.ClassCapacity || verdict.BucketKey != "" {
			t.Fatalf("verdict=%+v err=%v", verdict, err)
		}
	})

	t.Run("store dependency error is unknown not capacity", func(t *testing.T) {
		store := &recStore{reserveErr: errors.New("redis down")}
		verdict, err := NewEndpointQuota(store).Reserve(context.Background(), epWithRPM(7, 100))
		if err != nil || verdict == nil || verdict.Class != dispatch.ClassUnknown || !strings.Contains(verdict.Reason, "redis down") {
			t.Fatalf("verdict=%+v err=%v", verdict, err)
		}
	})
}

func TestEndpointQuotaAdapterReserveNoops(t *testing.T) {
	var nilAdapter *EndpointQuotaAdapter
	for name, adapter := range map[string]*EndpointQuotaAdapter{
		"nil adapter": nilAdapter,
		"nil store":   NewEndpointQuota(nil),
	} {
		t.Run(name, func(t *testing.T) {
			if verdict, err := adapter.Reserve(context.Background(), epWithRPM(1, 1)); err != nil || verdict != nil {
				t.Fatalf("verdict=%+v err=%v", verdict, err)
			}
		})
	}
	store := &recStore{}
	adapter := NewEndpointQuota(store)
	for _, endpoint := range []*domain.Endpoint{nil, {ID: 1}} {
		if verdict, err := adapter.Reserve(context.Background(), endpoint); err != nil || verdict != nil {
			t.Fatalf("endpoint=%+v verdict=%+v err=%v", endpoint, verdict, err)
		}
	}
	if len(store.reserved) != 0 {
		t.Fatalf("reserved=%v", store.reserved)
	}
}

func TestEndpointQuotaAdapterChargeUsage(t *testing.T) {
	store := &recStore{}
	adapter := NewEndpointQuota(store)
	ep := &domain.Endpoint{ID: 9, Quota: domain.QuotaConfig{TPM: uint32ptr(100)}}
	adapter.ChargeUsage(context.Background(), ep, &domain.Usage{Total: 12})
	if len(store.charged) != 1 || len(store.charged[0]) != 1 {
		t.Fatalf("charged=%v", store.charged)
	}
	bucket := store.charged[0][0]
	if bucket.Key != "rl:endpoint:9:tpm" || bucket.Limit != 100 || bucket.Cost != 12 {
		t.Fatalf("bucket=%+v", bucket)
	}

	before := len(store.charged)
	for _, call := range []struct {
		adapter *EndpointQuotaAdapter
		ep      *domain.Endpoint
		usage   *domain.Usage
	}{
		{nil, ep, &domain.Usage{Total: 1}},
		{NewEndpointQuota(nil), ep, &domain.Usage{Total: 1}},
		{adapter, nil, &domain.Usage{Total: 1}},
		{adapter, ep, nil},
		{adapter, ep, &domain.Usage{}},
		{adapter, &domain.Endpoint{ID: 10}, &domain.Usage{Total: 1}},
	} {
		call.adapter.ChargeUsage(context.Background(), call.ep, call.usage)
	}
	if len(store.charged) != before {
		t.Fatalf("no-op calls charged=%v", store.charged[before:])
	}
}

func TestEndpointQuotaAdapterReleaseNoopsAndIgnoresStoreError(t *testing.T) {
	var nilAdapter *EndpointQuotaAdapter
	nilAdapter.Release(context.Background(), epWithRPM(1, 1))
	NewEndpointQuota(nil).Release(context.Background(), epWithRPM(1, 1))

	store := &recStore{releaseErr: errors.New("redis down")}
	adapter := NewEndpointQuota(store)
	adapter.Release(context.Background(), nil)
	adapter.Release(context.Background(), &domain.Endpoint{ID: 1})
	adapter.Release(context.Background(), epWithRPM(1, 1))
	if len(store.released) != 1 {
		t.Fatalf("released=%v", store.released)
	}
}

func epWithRPM(id int64, rpm uint32) *domain.Endpoint {
	return &domain.Endpoint{ID: id, Quota: domain.QuotaConfig{RPM: &rpm}}
}

// Release must forward the endpoint's reserve buckets to store.ReleaseBatch.
func TestEndpointQuotaAdapter_ReleaseRollsBackReserveBuckets(t *testing.T) {
	st := &recStore{}
	q := NewEndpointQuota(st)

	q.Release(context.Background(), epWithRPM(7, 100))

	if len(st.released) != 1 {
		t.Fatalf("want 1 ReleaseBatch call, got %d", len(st.released))
	}
	if len(st.released[0]) == 0 {
		t.Fatalf("expected reserve buckets to be released, got none")
	}
	// buckets must be the endpoint's own reserve buckets (rl:endpoint:7:*)
	want := ratelimit.EndpointReserveBuckets(epWithRPM(7, 100))
	if len(st.released[0]) != len(want) {
		t.Errorf("released %d buckets, want %d", len(st.released[0]), len(want))
	}
}

// An endpoint with no quota configured releases nothing.
func TestEndpointQuotaAdapter_ReleaseNoQuotaNoop(t *testing.T) {
	st := &recStore{}
	q := NewEndpointQuota(st)
	q.Release(context.Background(), &domain.Endpoint{ID: 9})
	if len(st.released) != 0 {
		t.Errorf("endpoint without quota should release nothing, got %v", st.released)
	}
}
