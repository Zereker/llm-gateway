package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// SQLSubscriptionProvider is the sqlx implementation of SubscriptionProvider.
//
// **v0.3 queries the DB directly, no cache** — every M5 call runs a SELECT.
// It hits the uk_account_model (account_id, model_service_id) index directly, ~1ms.
// Add caching once volume actually demands it.
type SQLSubscriptionProvider struct {
	db *sqlx.DB
}

func NewSQLSubscriptionProvider(db *sqlx.DB) *SQLSubscriptionProvider {
	return &SQLSubscriptionProvider{db: db}
}

// Has implements SubscriptionProvider.Has.
//
// SELECT 1 is the cheapest existence check; it doesn't fetch any row fields.
// Condition: enabled = 1 AND deleted_at IS NULL (neither a soft-disable nor a
// soft-delete counts as subscribed).
func (p *SQLSubscriptionProvider) Has(ctx context.Context, accountID string, modelServiceID int64) (bool, error) {
	if accountID == "" {
		return false, errors.New("subscription: empty account_id")
	}
	if modelServiceID == 0 {
		return false, errors.New("subscription: empty model_service_id")
	}
	var one int
	err := p.db.GetContext(ctx, &one, p.db.Rebind(
		`SELECT 1 FROM account_model_subscriptions
		 WHERE account_id = ? AND model_service_id = ?
		   AND enabled = 1 AND deleted_at IS NULL
		 LIMIT 1`),
		accountID, modelServiceID,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("subscription: lookup: %w", err)
	}
	return true, nil
}

// Compile-time assertion.
var _ SubscriptionProvider = (*SQLSubscriptionProvider)(nil)
