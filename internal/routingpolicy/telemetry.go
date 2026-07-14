package routingpolicy

import (
	"context"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/selector"
)

type EndpointReader interface {
	ListForModel(ctx context.Context, model, group string) ([]*domain.Endpoint, error)
}

// SelectorTelemetryReader projects the selector's existing per-endpoint EMA
// store into routing signals. It introduces no second telemetry pipeline.
type SelectorTelemetryReader struct {
	endpoints EndpointReader
	stats     selector.EndpointStatsStore
}

func NewSelectorTelemetryReader(endpoints EndpointReader, stats selector.EndpointStatsStore) *SelectorTelemetryReader {
	return &SelectorTelemetryReader{endpoints: endpoints, stats: stats}
}

func (r *SelectorTelemetryReader) ForModel(ctx context.Context, model, group string) ([]EndpointTelemetry, error) {
	if r == nil || r.endpoints == nil || r.stats == nil {
		return nil, nil
	}

	endpoints, err := r.endpoints.ListForModel(ctx, model, group)
	if err != nil {
		return nil, err
	}

	out := make([]EndpointTelemetry, 0, len(endpoints))
	for _, endpoint := range endpoints {
		if endpoint == nil || !endpoint.Enabled {
			continue
		}

		stats := r.stats.Snapshot(ctx, endpoint.ID)
		out = append(out, EndpointTelemetry{
			LatencyMs: stats.LatencyMs, SuccessRate: stats.SuccessRate,
			SampleCount: stats.SampleCount, Updated: stats.Updated,
		})
	}

	return out, nil
}

var _ TelemetryReader = (*SelectorTelemetryReader)(nil)
