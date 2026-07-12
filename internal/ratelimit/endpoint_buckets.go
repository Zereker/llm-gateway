package ratelimit

import (
	"fmt"
	"time"

	"github.com/zereker/llm-gateway/internal/domain"
)

// EndpointReserveBuckets expands an endpoint quota into RPM + RPS reserve buckets
// (for pre-charge, cost=1).
//
// Naming rule (docs/04 §10): rl:endpoint:<id>:rpm / rl:endpoint:<id>:rps
//
// **Lives in internal/ratelimit**: deriving bucket keys is ratelimit's own domain knowledge; having it
// in internal/selector was legacy from before, moved here in v0.6 (which also resolved a
// selector ↔ ratelimit import cycle).
func EndpointReserveBuckets(ep *domain.Endpoint) []Bucket {
	return buildEndpointReserveBuckets(ep, 1)
}

// EndpointTPMChargeBucket charges the endpoint TPM bucket after a successful response.
//
// cost is passed in by the caller as rc.Usage.Total. Returns nil if the endpoint has no TPM configured.
func EndpointTPMChargeBucket(ep *domain.Endpoint, cost uint32) *Bucket {
	if ep == nil {
		return nil
	}

	q := ep.Quota
	if q.TPM == nil || *q.TPM == 0 || cost == 0 {
		return nil
	}

	return &Bucket{
		Key:    fmt.Sprintf("rl:endpoint:%d:tpm", ep.ID),
		Limit:  *q.TPM,
		Cost:   cost,
		Window: time.Minute,
	}
}

// buildEndpointReserveBuckets converts an endpoint quota → RPM / RPS buckets.
// The rpsCost parameter lets selector's LimitReadFilter use a finer-grained budget (peek without charging).
func buildEndpointReserveBuckets(ep *domain.Endpoint, rpsCost uint32) []Bucket {
	if ep == nil {
		return nil
	}

	q := ep.Quota

	var buckets []Bucket
	if q.RPM != nil && *q.RPM > 0 {
		buckets = append(buckets, Bucket{
			Key:    fmt.Sprintf("rl:endpoint:%d:rpm", ep.ID),
			Limit:  *q.RPM,
			Cost:   rpsCost,
			Window: time.Minute,
		})
	}

	if q.RPS != nil && *q.RPS > 0 {
		buckets = append(buckets, Bucket{
			Key:    fmt.Sprintf("rl:endpoint:%d:rps", ep.ID),
			Limit:  *q.RPS,
			Cost:   rpsCost,
			Window: time.Second,
		})
	}

	return buckets
}
