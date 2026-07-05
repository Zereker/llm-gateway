package repo

import (
	"context"
)

// ModelServiceReader is the dependency of the gateway data plane (M5
// ModelService middleware).
//
// **v0.3 change**: dropped the accountID parameter — model_services is now a
// global catalog, no longer per-account; model visibility goes through
// SubscriptionProvider.
//
// Only read methods are declared here — writes go straight through SQL (the
// model_services table).
//
// Implementations MUST be safe for concurrent use (called by multiple gin
// handler goroutines at once).
type ModelServiceReader interface {
	GetByModel(c context.Context, model string) (*ModelService, error)
	List(c context.Context) ([]*ModelService, error)
}
