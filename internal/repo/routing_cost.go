package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/zereker/llm-gateway/internal/domain"
)

// RoutingCostReader supplies compact operator-cost snapshots to routing. It
// does not expose customer pricing, discounts, invoices, or billing services.
type RoutingCostReader interface {
	GetActive(ctx context.Context, modelServiceID int64) (*domain.RoutingCostProfile, error)
}

type routingCostRow struct {
	ProfileID                     string `db:"profile_id"`
	Version                       uint64 `db:"version"`
	ModelServiceID                int64  `db:"model_service_id"`
	InputMicrousdPerMillionToken  uint64 `db:"input_microusd_per_million_tokens"`
	OutputMicrousdPerMillionToken uint64 `db:"output_microusd_per_million_tokens"`
}

type SQLRoutingCostReader struct{ db *sqlx.DB }

func NewSQLRoutingCostReader(db *sqlx.DB) *SQLRoutingCostReader { return &SQLRoutingCostReader{db: db} }

func (r *SQLRoutingCostReader) GetActive(ctx context.Context, modelServiceID int64) (*domain.RoutingCostProfile, error) {
	var row routingCostRow

	err := r.db.GetContext(ctx, &row, `SELECT profile_id, version, model_service_id,
		input_microusd_per_million_tokens, output_microusd_per_million_tokens
		FROM routing_cost_profiles
		WHERE model_service_id = ? AND enabled = 1 AND deleted_at IS NULL
		ORDER BY version DESC LIMIT 1`, modelServiceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("routing cost profile: resolve: %w", err)
	}

	return &domain.RoutingCostProfile{
		Ref:                           domain.RoutingCostProfileRef{ID: row.ProfileID, Version: row.Version},
		ModelServiceID:                row.ModelServiceID,
		InputMicrousdPerMillionToken:  row.InputMicrousdPerMillionToken,
		OutputMicrousdPerMillionToken: row.OutputMicrousdPerMillionToken,
	}, nil
}

type CachedRoutingCostReader struct {
	inner    RoutingCostReader
	cache    *TTLCache[string, *domain.RoutingCostProfile]
	negative *TTLCache[string, struct{}]
}

func NewCachedRoutingCostReader(inner RoutingCostReader, capacity int, ttl time.Duration, metrics Metrics) *CachedRoutingCostReader {
	return &CachedRoutingCostReader{
		inner:    inner,
		cache:    NewTTLCache[string, *domain.RoutingCostProfile](capacity, ttl).WithMetrics("routing_cost_profiles", metrics),
		negative: NewTTLCache[string, struct{}](capacity, negativeTTL).WithMetrics("routing_cost_profiles_negative", metrics),
	}
}

func (r *CachedRoutingCostReader) GetActive(ctx context.Context, modelServiceID int64) (*domain.RoutingCostProfile, error) {
	key := strconv.FormatInt(modelServiceID, 10)
	if _, missing := r.negative.Get(key); missing {
		return nil, nil
	}

	profile, err := r.cache.GetOrLoad(ctx, key, func(ctx context.Context) (*domain.RoutingCostProfile, bool, error) {
		value, loadErr := r.inner.GetActive(ctx, modelServiceID)
		return value, loadErr == nil && value != nil, loadErr
	})
	if err == nil && profile == nil {
		r.negative.Set(key, struct{}{})
	}

	return profile, err
}

func (r *CachedRoutingCostReader) EvictAll() { r.cache.Purge(); r.negative.Purge() }

var _ RoutingCostReader = (*SQLRoutingCostReader)(nil)
var _ RoutingCostReader = (*CachedRoutingCostReader)(nil)
