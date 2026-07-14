package repo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zereker/llm-gateway/internal/domain"
)

func TestSQLRoutingCostReaderReturnsLatestActiveProfile(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	res, err := db.ExecContext(ctx, `INSERT INTO model_services (service_id, model) VALUES ('svc/cost', 'cost-model')`)
	if err != nil {
		t.Fatal(err)
	}
	modelID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.ExecContext(ctx, `INSERT INTO routing_cost_profiles
		(profile_id, version, model_service_id, input_microusd_per_million_tokens,
		 output_microusd_per_million_tokens, enabled)
		VALUES ('rcp_cost', 1, ?, 10, 20, 0), ('rcp_cost', 2, ?, 30, 40, 1)`, modelID, modelID)
	if err != nil {
		t.Fatal(err)
	}

	reader := NewSQLRoutingCostReader(db)
	profile, err := reader.GetActive(ctx, modelID)
	if err != nil {
		t.Fatal(err)
	}
	if profile == nil || profile.Ref.ID != "rcp_cost" || profile.Ref.Version != 2 ||
		profile.ModelServiceID != modelID || profile.InputMicrousdPerMillionToken != 30 ||
		profile.OutputMicrousdPerMillionToken != 40 {
		t.Fatalf("profile = %+v", profile)
	}

	missing, err := reader.GetActive(ctx, modelID+1)
	if err != nil || missing != nil {
		t.Fatalf("missing profile = %+v, err=%v", missing, err)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.GetActive(ctx, modelID); err == nil {
		t.Fatal("GetActive succeeded on a closed database")
	}
}

type routingCostReaderStub struct {
	profile *domain.RoutingCostProfile
	err     error
	calls   int
}

func (s *routingCostReaderStub) GetActive(context.Context, int64) (*domain.RoutingCostProfile, error) {
	s.calls++

	return s.profile, s.err
}

func TestCachedRoutingCostReaderCachesNegativePositiveAndErrors(t *testing.T) {
	ctx := context.Background()
	inner := &routingCostReaderStub{}
	reader := NewCachedRoutingCostReader(inner, 4, time.Minute, nil)

	for range 2 {
		profile, err := reader.GetActive(ctx, 7)
		if err != nil || profile != nil {
			t.Fatalf("negative read = %+v, err=%v", profile, err)
		}
	}
	if inner.calls != 1 {
		t.Fatalf("negative load calls=%d, want 1", inner.calls)
	}

	inner.profile = &domain.RoutingCostProfile{Ref: domain.RoutingCostProfileRef{ID: "rcp_7", Version: 1}, ModelServiceID: 7}
	reader.EvictAll()
	for range 2 {
		profile, err := reader.GetActive(ctx, 7)
		if err != nil || profile == nil || profile.Ref.ID != "rcp_7" {
			t.Fatalf("positive read = %+v, err=%v", profile, err)
		}
	}
	if inner.calls != 2 {
		t.Fatalf("positive load calls=%d, want 2", inner.calls)
	}

	wantErr := errors.New("cost store unavailable")
	errorReader := NewCachedRoutingCostReader(&routingCostReaderStub{err: wantErr}, 4, time.Minute, nil)
	if _, err := errorReader.GetActive(ctx, 8); !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
}
