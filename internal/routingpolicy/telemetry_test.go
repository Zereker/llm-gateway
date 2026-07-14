package routingpolicy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/selector"
)

type telemetryEndpointReader struct {
	endpoints []*domain.Endpoint
	err       error
}

func (r telemetryEndpointReader) ListForModel(context.Context, string, string) ([]*domain.Endpoint, error) {
	return r.endpoints, r.err
}

type telemetryStatsStore struct {
	stats map[int64]selector.EndpointStats
}

func (s telemetryStatsStore) Record(context.Context, int64, selector.Result) {}

func (s telemetryStatsStore) Snapshot(_ context.Context, id int64) selector.EndpointStats {
	return s.stats[id]
}

func TestSelectorTelemetryReaderProjectsEnabledEndpointStats(t *testing.T) {
	now := time.Now().UTC()
	reader := NewSelectorTelemetryReader(telemetryEndpointReader{endpoints: []*domain.Endpoint{
		nil,
		{ID: 1, Enabled: false},
		{ID: 2, Enabled: true},
	}}, telemetryStatsStore{stats: map[int64]selector.EndpointStats{
		2: {LatencyMs: 125, SuccessRate: 0.9, SampleCount: 12, Updated: now},
	}})

	snapshots, err := reader.ForModel(context.Background(), "model", "group")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].LatencyMs != 125 || snapshots[0].SuccessRate != 0.9 ||
		snapshots[0].SampleCount != 12 || !snapshots[0].Updated.Equal(now) {
		t.Fatalf("snapshots = %+v", snapshots)
	}
}

func TestSelectorTelemetryReaderNeutralAndErrorPaths(t *testing.T) {
	ctx := context.Background()

	for name, reader := range map[string]*SelectorTelemetryReader{
		"nil receiver":  nil,
		"nil endpoints": NewSelectorTelemetryReader(nil, telemetryStatsStore{}),
		"nil stats":     NewSelectorTelemetryReader(telemetryEndpointReader{}, nil),
	} {
		t.Run(name, func(t *testing.T) {
			snapshots, err := reader.ForModel(ctx, "model", "group")
			if err != nil || snapshots != nil {
				t.Fatalf("snapshots=%v err=%v", snapshots, err)
			}
		})
	}

	wantErr := errors.New("endpoint store unavailable")
	reader := NewSelectorTelemetryReader(telemetryEndpointReader{err: wantErr}, telemetryStatsStore{})
	if _, err := reader.ForModel(ctx, "model", "group"); !errors.Is(err, wantErr) {
		t.Fatalf("error=%v, want %v", err, wantErr)
	}
}
