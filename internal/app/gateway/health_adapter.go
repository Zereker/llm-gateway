package gateway

import (
	"context"

	"github.com/zereker/llm-gateway/pkg/health"
	"github.com/zereker/llm-gateway/pkg/selector"
)

type healthFeedbackAdapter struct{ stats selector.EndpointStatsStore }

func (a healthFeedbackAdapter) RecordHealth(ctx context.Context, endpointID int64, result health.Result) {
	a.stats.Record(ctx, endpointID, selector.Result{
		Class: result.Class, HTTPCode: result.HTTPCode, Reason: result.Reason, Latency: result.Latency,
	})
}
