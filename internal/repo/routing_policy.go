package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/zereker/llm-gateway/internal/domain"
)

// RoutingPolicyReader loads the most-specific immutable policy snapshot.
type RoutingPolicyReader interface {
	GetEffective(ctx context.Context, accountID, projectID, virtualModel string) (*domain.RoutingPolicy, error)
}

type routingPolicyRow struct {
	PolicyID     string          `db:"policy_id"`
	Version      uint64          `db:"version"`
	ScopeKind    string          `db:"scope_kind"`
	ScopeID      string          `db:"scope_id"`
	VirtualModel string          `db:"virtual_model"`
	RuleJSON     json.RawMessage `db:"rule_json"`
}

type routingPolicyRule struct {
	MaxAttempts int                             `json:"max_attempts,omitempty"`
	Constraints domain.RoutingConstraints       `json:"constraints,omitempty"`
	Objectives  domain.RoutingObjectives        `json:"objectives,omitempty"`
	Candidates  []domain.RoutingPolicyCandidate `json:"candidates"`
}

// SQLRoutingPolicyReader resolves project > account > global in one query.
type SQLRoutingPolicyReader struct{ db *sqlx.DB }

func NewSQLRoutingPolicyReader(db *sqlx.DB) *SQLRoutingPolicyReader {
	return &SQLRoutingPolicyReader{db: db}
}

func (r *SQLRoutingPolicyReader) GetEffective(
	ctx context.Context,
	accountID, projectID, virtualModel string,
) (*domain.RoutingPolicy, error) {
	if virtualModel == "" {
		return nil, fmt.Errorf("routing policy: virtual model is required")
	}

	var row routingPolicyRow

	err := r.db.GetContext(ctx, &row, r.db.Rebind(
		`SELECT policy_id, version, scope_kind, scope_id, virtual_model, rule_json
		 FROM routing_policies
		 WHERE virtual_model = ? AND enabled = 1 AND deleted_at IS NULL
		   AND ((? <> '' AND scope_kind = 'project' AND scope_id = ?)
		     OR (scope_kind = 'account' AND scope_id = ?)
		     OR (scope_kind = 'global' AND scope_id = ''))
		 ORDER BY CASE scope_kind WHEN 'project' THEN 3 WHEN 'account' THEN 2 ELSE 1 END DESC,
		          version DESC
		 LIMIT 1`),
		virtualModel, projectID, projectID, accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("routing policy: resolve: %w", err)
	}

	var rule routingPolicyRule
	if err := json.Unmarshal(row.RuleJSON, &rule); err != nil {
		return nil, fmt.Errorf("routing policy %s@%d: decode: %w", row.PolicyID, row.Version, err)
	}

	return &domain.RoutingPolicy{
		Ref: domain.RoutingPolicyRef{
			ID:      row.PolicyID,
			Version: row.Version,
			Scope:   domain.RoutingScope{Kind: domain.RoutingScopeKind(row.ScopeKind), ID: row.ScopeID},
		},
		VirtualModel: row.VirtualModel,
		MaxAttempts:  rule.MaxAttempts,
		Constraints:  rule.Constraints,
		Objectives:   rule.Objectives,
		Candidates:   rule.Candidates,
	}, nil
}

// CachedRoutingPolicyReader caches complete compiled snapshots. Negative
// results use a short cache so newly-created virtual models become visible
// quickly even when invalidation is unavailable.
type CachedRoutingPolicyReader struct {
	inner    RoutingPolicyReader
	cache    *TTLCache[string, *domain.RoutingPolicy]
	negative *TTLCache[string, struct{}]
}

func NewCachedRoutingPolicyReader(
	inner RoutingPolicyReader,
	capacity int,
	ttl time.Duration,
	metrics Metrics,
) *CachedRoutingPolicyReader {
	return &CachedRoutingPolicyReader{
		inner:    inner,
		cache:    NewTTLCache[string, *domain.RoutingPolicy](capacity, ttl).WithMetrics("routing_policies", metrics),
		negative: NewTTLCache[string, struct{}](capacity, negativeTTL).WithMetrics("routing_policies_negative", metrics),
	}
}

func (r *CachedRoutingPolicyReader) GetEffective(
	ctx context.Context,
	accountID, projectID, virtualModel string,
) (*domain.RoutingPolicy, error) {
	key := accountID + "\x00" + projectID + "\x00" + virtualModel
	if _, missing := r.negative.Get(key); missing {
		return nil, nil
	}

	policy, err := r.cache.GetOrLoad(ctx, key, func(ctx context.Context) (*domain.RoutingPolicy, bool, error) {
		value, err := r.inner.GetEffective(ctx, accountID, projectID, virtualModel)
		return value, err == nil && value != nil, err
	})
	if err == nil && policy == nil {
		r.negative.Set(key, struct{}{})
	}

	return policy, err
}

// EvictAll is intentionally coarse: global policy changes can affect every
// account cache key. The cache is small and misses are singleflight-protected.
func (r *CachedRoutingPolicyReader) EvictAll() {
	r.cache.Purge()
	r.negative.Purge()
}

var _ RoutingPolicyReader = (*SQLRoutingPolicyReader)(nil)
var _ RoutingPolicyReader = (*CachedRoutingPolicyReader)(nil)
