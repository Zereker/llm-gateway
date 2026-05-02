package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// SQLEndpointReader 是 EndpointReader 的 sqlx 实现。
type SQLEndpointReader struct {
	db *sqlx.DB
}

// NewSQLEndpointReader 用现成 *sqlx.DB 构造。
func NewSQLEndpointReader(db *sqlx.DB) *SQLEndpointReader {
	return &SQLEndpointReader{db: db}
}

const epColumns = `id, vendor, url, api_key, group_name, model, weight, rpm, tpm, rps, capabilities, extra`

// PickForModel 实现 EndpointReader.PickForModel。
//
// v0.1：按 (model, group_name) 取第一行。无加权 / 无 Filter / 无 Cooldown。
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
		 ORDER BY id LIMIT 1`),
		model, group)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("endpoint: no endpoint for model %q in group %q", model, group)
		}
		return nil, fmt.Errorf("endpoint: pick: %w", err)
	}
	return &ep, nil
}

// GetByID 实现 EndpointReader.GetByID。
func (r *SQLEndpointReader) GetByID(ctx context.Context, id string) (*Endpoint, error) {
	if id == "" {
		return nil, errors.New("endpoint: empty id")
	}
	var ep Endpoint
	err := r.db.GetContext(ctx, &ep, r.db.Rebind(
		`SELECT `+epColumns+` FROM endpoints WHERE id = ?`), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("endpoint: not found: %s", id)
		}
		return nil, fmt.Errorf("endpoint: get by id: %w", err)
	}
	return &ep, nil
}

// List 实现 EndpointReader.List。
func (r *SQLEndpointReader) List(ctx context.Context) ([]*Endpoint, error) {
	var rows []Endpoint
	if err := r.db.SelectContext(ctx, &rows,
		`SELECT `+epColumns+` FROM endpoints ORDER BY id`); err != nil {
		return nil, fmt.Errorf("endpoint: list: %w", err)
	}
	out := make([]*Endpoint, len(rows))
	for i := range rows {
		out[i] = &rows[i]
	}
	return out, nil
}

// 编译期断言。
var _ EndpointReader = (*SQLEndpointReader)(nil)
