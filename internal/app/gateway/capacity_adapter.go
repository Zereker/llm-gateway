package gateway

import (
	"context"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
)

// endpointCapacityAdapter translates selector's availability question into
// rate-limit bucket snapshots at the composition boundary.
type endpointCapacityAdapter struct{ store ratelimit.Store }

func (a endpointCapacityAdapter) Available(ctx context.Context, endpoints []*domain.Endpoint) (map[int64]bool, error) {
	available := make(map[int64]bool, len(endpoints))
	type slot struct{ endpointID int64 }
	var slots []slot
	var buckets []ratelimit.Bucket
	for _, endpoint := range endpoints {
		endpointBuckets := ratelimit.EndpointReserveBuckets(endpoint)
		if len(endpointBuckets) == 0 {
			available[endpoint.ID] = true
		}
		for _, bucket := range endpointBuckets {
			buckets = append(buckets, bucket)
			slots = append(slots, slot{endpointID: endpoint.ID})
		}
	}
	if len(buckets) == 0 {
		return available, nil
	}
	states, err := a.store.SnapshotBatch(ctx, buckets)
	if err != nil {
		return nil, err
	}
	for i, state := range states {
		id := slots[i].endpointID
		if _, seen := available[id]; !seen {
			available[id] = true
		}
		if state.Used+1 > state.Limit {
			available[id] = false
		}
	}
	return available, nil
}
