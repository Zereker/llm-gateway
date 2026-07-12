package repo

import "context"

// SubscriptionProvider is used by M5 ModelService middleware: determines
// whether an account is subscribed to a given model_service.
//
// **fail-fast semantics**: after M5 obtains the model_service, it must query
// this table to confirm the subscription; not found -> 403. This is the core
// control point for model visibility on a SaaS platform ("which account can
// see which model").
//
// Implementations MUST be safe for concurrent use (called by multiple gin
// handler goroutines at once).
type SubscriptionProvider interface {
	// Has determines whether (account_id, model_service_id) is subscribed,
	// enabled, and not soft-deleted. account_id is a legacy column name;
	// semantically it's the account pin.
	// Returns (true, nil) = subscribed; (false, nil) = not subscribed;
	// (_, err) = SQL error.
	Has(ctx context.Context, accountID string, modelServiceID int64) (bool, error)
}
