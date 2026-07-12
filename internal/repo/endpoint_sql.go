package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// SQLEndpointReader is the sqlx implementation of EndpointReader.
//
// **v0.3 change**: dropped account_id (endpoints are a global upstream pool;
// add a nullable account_id if BYOK etc. is actually needed later).
type SQLEndpointReader struct {
	db *sqlx.DB
}

// NewSQLEndpointReader builds one from an existing *sqlx.DB.
func NewSQLEndpointReader(db *sqlx.DB) *SQLEndpointReader {
	return &SQLEndpointReader{db: db}
}

const epColumns = `id, name, vendor, protocol, model, group_name, weight, enabled,
	auth, routing, quota, capabilities, quirks, extra,
	created_at, updated_at, deleted_at`

// ListForModel implements EndpointReader.ListForModel.
//
// Returns all endpoints matching (model, group_name) that are enabled and not
// deleted, sorted by weight DESC, with id ASC as a stable tiebreaker within
// the same weight. M7 LimitReadFilter iterates over these in order; the first
// one not over its endpoint quota is selected.
//
// Not found -> (empty slice, nil); the caller aborts with 503 itself.
func (r *SQLEndpointReader) ListForModel(ctx context.Context, model, group string) ([]*Endpoint, error) {
	if model == "" {
		return nil, errors.New("endpoint: empty model name")
	}

	if group == "" {
		group = "default"
	}

	var rows []Endpoint
	if err := r.db.SelectContext(ctx, &rows, r.db.Rebind(
		`SELECT `+epColumns+`
		 FROM endpoints
		 WHERE model = ? AND group_name = ?
		   AND enabled = 1 AND deleted_at IS NULL
		 ORDER BY weight DESC, id ASC`),
		model, group,
	); err != nil {
		return nil, fmt.Errorf("endpoint: list for model: %w", err)
	}

	out := make([]*Endpoint, len(rows))
	for i := range rows {
		out[i] = &rows[i]
	}

	return out, nil
}

// PickForModel implements EndpointReader.PickForModel.
//
// v0.1: takes the first row matching (model, group_name) + enabled = 1 +
// deleted_at IS NULL. No weighting / no Filter / no Cooldown (internal/schedule
// takes over starting v0.5+).
func (r *SQLEndpointReader) PickForModel(ctx context.Context, model, group string) (*Endpoint, error) {
	if model == "" {
		return nil, errors.New("endpoint: empty model name")
	}

	if group == "" {
		group = "default"
	}

	var ep Endpoint

	err := r.db.GetContext(ctx, &ep, r.db.Rebind(
		`SELECT `+epColumns+`
		 FROM endpoints
		 WHERE model = ? AND group_name = ?
		   AND enabled = 1 AND deleted_at IS NULL
		 ORDER BY id LIMIT 1`),
		model, group)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("endpoint: no endpoint for model=%q group=%q", model, group)
		}

		return nil, fmt.Errorf("endpoint: pick: %w", err)
	}

	return &ep, nil
}

// GetByID implements EndpointReader.GetByID. BIGINT id.
func (r *SQLEndpointReader) GetByID(ctx context.Context, id int64) (*Endpoint, error) {
	if id == 0 {
		return nil, errors.New("endpoint: empty id")
	}

	var ep Endpoint

	err := r.db.GetContext(ctx, &ep, r.db.Rebind(
		`SELECT `+epColumns+` FROM endpoints
		 WHERE id = ? AND deleted_at IS NULL`),
		id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("endpoint: not found: id=%d", id)
		}

		return nil, fmt.Errorf("endpoint: get by id: %w", err)
	}

	return &ep, nil
}

// List implements EndpointReader.List; lists all non-deleted endpoints.
func (r *SQLEndpointReader) List(ctx context.Context) ([]*Endpoint, error) {
	var rows []Endpoint
	if err := r.db.SelectContext(ctx, &rows,
		`SELECT `+epColumns+` FROM endpoints
		 WHERE deleted_at IS NULL ORDER BY id`,
	); err != nil {
		return nil, fmt.Errorf("endpoint: list: %w", err)
	}

	out := make([]*Endpoint, len(rows))
	for i := range rows {
		out[i] = &rows[i]
	}

	return out, nil
}

// Compile-time assertion.
var _ EndpointReader = (*SQLEndpointReader)(nil)
