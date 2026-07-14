package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/zereker/llm-gateway/internal/policy"
)

type PolicyDefinitionReader interface {
	Resolve(ctx context.Context, subject policy.Subject) (*policy.Definition, error)
}

type SQLPolicyDefinitionReader struct{ db *sqlx.DB }

func NewSQLPolicyDefinitionReader(db *sqlx.DB) *SQLPolicyDefinitionReader {
	return &SQLPolicyDefinitionReader{db: db}
}

func (r *SQLPolicyDefinitionReader) Resolve(ctx context.Context, subject policy.Subject) (*policy.Definition, error) {
	var row struct {
		PolicyID       string `db:"policy_id"`
		Version        uint64 `db:"version"`
		ScopeKind      string `db:"scope_kind"`
		ScopeID        string `db:"scope_id"`
		Name           string `db:"name"`
		InputEnabled   bool   `db:"input_enabled"`
		OutputMode     string `db:"output_mode"`
		MaxBufferBytes int    `db:"max_buffer_bytes"`
	}

	err := r.db.GetContext(ctx, &row, r.db.Rebind(
		`SELECT d.policy_id, d.version, b.scope_kind, b.scope_id, d.name,
		        d.input_enabled, d.output_mode, d.max_buffer_bytes
		 FROM policy_bindings b
		 JOIN policy_definitions d ON d.policy_id = b.policy_id AND d.version = b.policy_version
		 WHERE b.enabled = 1 AND b.deleted_at IS NULL
		   AND d.enabled = 1 AND d.deleted_at IS NULL
		   AND ((? <> '' AND b.scope_kind = 'api_key' AND b.scope_id = ?)
		     OR (? <> '' AND b.scope_kind = 'project' AND b.scope_id = ?)
		     OR (b.scope_kind = 'account' AND b.scope_id = ?)
		     OR (b.scope_kind = 'global' AND b.scope_id = ''))
		 ORDER BY CASE b.scope_kind WHEN 'api_key' THEN 4 WHEN 'project' THEN 3 WHEN 'account' THEN 2 ELSE 1 END DESC
		 LIMIT 1`),
		subject.APIKeyID, subject.APIKeyID, subject.ProjectID, subject.ProjectID, subject.AccountID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("policy definition: resolve: %w", err)
	}

	definition := &policy.Definition{
		Ref: policy.PolicyRef{
			ID: row.PolicyID, Version: row.Version,
			Scope: policy.Scope{Kind: policy.ScopeKind(row.ScopeKind), ID: row.ScopeID},
		},
		Name: row.Name, InputEnabled: row.InputEnabled,
		OutputMode: policy.OutputMode(row.OutputMode), MaxBufferBytes: row.MaxBufferBytes,
	}
	if err := definition.Validate(); err != nil {
		return nil, fmt.Errorf("policy definition %s@%d: %w", row.PolicyID, row.Version, err)
	}

	return definition, nil
}

type CachedPolicyDefinitionReader struct {
	inner    PolicyDefinitionReader
	cache    *TTLCache[string, *policy.Definition]
	negative *TTLCache[string, struct{}]
}

func NewCachedPolicyDefinitionReader(inner PolicyDefinitionReader, capacity int, ttl time.Duration, metrics Metrics) *CachedPolicyDefinitionReader {
	return &CachedPolicyDefinitionReader{
		inner:    inner,
		cache:    NewTTLCache[string, *policy.Definition](capacity, ttl).WithMetrics("policy_definitions", metrics),
		negative: NewTTLCache[string, struct{}](capacity, negativeTTL).WithMetrics("policy_definitions_negative", metrics),
	}
}

func (r *CachedPolicyDefinitionReader) Resolve(ctx context.Context, subject policy.Subject) (*policy.Definition, error) {
	key := subject.AccountID + "\x00" + subject.ProjectID + "\x00" + subject.APIKeyID
	if _, missing := r.negative.Get(key); missing {
		return nil, nil
	}

	definition, err := r.cache.GetOrLoad(ctx, key, func(ctx context.Context) (*policy.Definition, bool, error) {
		value, loadErr := r.inner.Resolve(ctx, subject)

		return value, loadErr == nil && value != nil, loadErr
	})
	if err == nil && definition == nil {
		r.negative.Set(key, struct{}{})
	}

	return definition, err
}

func (r *CachedPolicyDefinitionReader) EvictAll() {
	r.cache.Purge()
	r.negative.Purge()
}

var _ policy.Resolver = (*SQLPolicyDefinitionReader)(nil)
var _ policy.Resolver = (*CachedPolicyDefinitionReader)(nil)
