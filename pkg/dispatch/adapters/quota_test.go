package adapters

import (
	"context"
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
)

// recStore records ReleaseBatch calls and allows reserves.
type recStore struct {
	released [][]ratelimit.Bucket
}

func (s *recStore) ReserveBatch(context.Context, []ratelimit.Bucket) (bool, *ratelimit.BucketViolation, error) {
	return true, nil, nil
}
func (s *recStore) ChargeBatch(context.Context, []ratelimit.Bucket) ([]ratelimit.BucketChargeResult, error) {
	return nil, nil
}
func (s *recStore) SnapshotBatch(context.Context, []ratelimit.Bucket) ([]ratelimit.BucketState, error) {
	return nil, nil
}
func (s *recStore) ReleaseBatch(_ context.Context, b []ratelimit.Bucket) error {
	s.released = append(s.released, b)
	return nil
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
