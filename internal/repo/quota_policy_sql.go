package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// QuotaPolicyProvider is used by M6 RateLimit middleware: loads a rate-limit
// policy by ID.
//
// Only read methods are declared here — writes go straight through SQL (the quota_policies table).
//
// **v0.5 queries the DB directly, no cache** — M6 does 2 lookups per request
// (account + apikey policy). Add an LRU + TTL once volume actually demands
// it; the schema stays the same.
//
// Implementations MUST be safe for concurrent use.
type QuotaPolicyProvider interface {
	// GetByID fetches the given policy; not found / soft-deleted / disabled
	// all return (nil, nil), letting M6 treat it as "this layer isn't
	// limited" — avoiding a deadlock for all referencing callers when the
	// deployer temporarily disables a policy.
	GetByID(ctx context.Context, id int64) (*QuotaPolicy, error)
}

// SQLQuotaPolicyProvider is the sqlx implementation.
type SQLQuotaPolicyProvider struct {
	db *sqlx.DB
}

func NewSQLQuotaPolicyProvider(db *sqlx.DB) *SQLQuotaPolicyProvider {
	return &SQLQuotaPolicyProvider{db: db}
}

const qpColumns = `id, name, description, rule_json, enabled,
	created_at, updated_at, deleted_at`

// GetByID implements QuotaPolicyProvider.GetByID.
//
// Not found / soft-deleted / disabled all return (nil, nil). M6 treats a nil
// result as "this layer isn't limited".
func (p *SQLQuotaPolicyProvider) GetByID(ctx context.Context, id int64) (*QuotaPolicy, error) {
	if id == 0 {
		return nil, nil
	}

	var pv QuotaPolicy

	err := p.db.GetContext(ctx, &pv, p.db.Rebind(
		`SELECT `+qpColumns+` FROM quota_policies
		 WHERE id = ? AND enabled = 1 AND deleted_at IS NULL
		 LIMIT 1`),
		id,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}

		return nil, fmt.Errorf("quota_policy: get by id: %w", err)
	}

	return &pv, nil
}

// Compile-time assertion.
var _ QuotaPolicyProvider = (*SQLQuotaPolicyProvider)(nil)
