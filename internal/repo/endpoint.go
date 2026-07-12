package repo

import (
	"context"
)

// EndpointReader is the endpoints read interface for the gateway data plane
// (M7 Schedule middleware).
//
// **v0.3 change**: dropped the accountID parameter — endpoints are now a
// global upstream pool, no longer per-account. Add back a nullable
// account_id filter later if BYOK (account brings its own endpoint) is needed.
//
// Writes go straight through SQL (the endpoints table) — this repo ships no control plane.
//
// Implementations MUST be safe for concurrent use (called by multiple gin
// handler goroutines at once).
type EndpointReader interface {
	// ListForModel returns all candidate endpoints matching (model, group),
	// sorted by weight DESC. M7 LimitReadFilter iterates over this to check
	// endpoint quota (the first one not over limit is selected). Returns an
	// empty slice + nil error when no candidate is found; M7 aborts with 503 itself.
	ListForModel(c context.Context, model, group string) ([]*Endpoint, error)

	// PickForModel selects the first endpoint matching (model, group); used
	// by M7's v0.1 simplified path. Does not participate in quota / cooldown /
	// weight filtering — that's done in the ListForModel + Filter chain.
	// Returns an error when none is found.
	PickForModel(c context.Context, model, group string) (*Endpoint, error)

	// GetByID fetches exactly one record by id.
	GetByID(c context.Context, id int64) (*Endpoint, error)

	// List returns all non-deleted endpoints (used by health probes / inspection).
	List(c context.Context) ([]*Endpoint, error)
}
