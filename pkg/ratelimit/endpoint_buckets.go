package ratelimit

import (
	"fmt"
	"time"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// EndpointReserveBuckets 把 endpoint quota 展开成 RPM + RPS reserve bucket
// （前扣用，cost=1）。
//
// 命名规则（docs/04 §10）：rl:endpoint:<id>:rpm / rl:endpoint:<id>:rps
//
// **归属在 pkg/ratelimit**：bucket key 派生是 ratelimit 自身的领域知识；
// 之前在 pkg/selector 是历史遗留，v0.6 归位（同时解 selector ↔ ratelimit 循环）。
func EndpointReserveBuckets(ep *domain.Endpoint) []Bucket {
	return buildEndpointReserveBuckets(ep, 1)
}

// EndpointTPMChargeBucket 成功响应后 charge endpoint TPM bucket。
//
// cost 由调用方传入 rc.Usage.Total。endpoint 没配 TPM 时返 nil。
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

// buildEndpointReserveBuckets endpoint quota → RPM / RPS buckets。
// rpsCost 参数让 selector 的 LimitReadFilter 用更细粒度（peek 不扣）的预算。
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
