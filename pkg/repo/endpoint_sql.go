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

const epColumns = `tenant_id, id, vendor, url, api_key, group_name, model, weight, rpm, tpm, rps, capabilities, extra`

// PickForModel 实现 EndpointReader.PickForModel。
//
// v0.1：按 (tenant_id, model, group_name) 取第一行。无加权 / 无 Filter / 无 Cooldown。
func (r *SQLEndpointReader) PickForModel(ctx context.Context, tenantID, model, group string) (*Endpoint, error) {
	if tenantID == "" {
		return nil, errors.New("endpoint: empty tenant_id")
	}
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
		 WHERE tenant_id = ? AND model = ? AND group_name = ?
		 ORDER BY id LIMIT 1`),
		tenantID, model, group)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("endpoint: no endpoint for tenant=%s model=%q group=%q", tenantID, model, group)
		}
		return nil, fmt.Errorf("endpoint: pick: %w", err)
	}
	return &ep, nil
}

// GetByID 实现 EndpointReader.GetByID。
func (r *SQLEndpointReader) GetByID(ctx context.Context, tenantID, id string) (*Endpoint, error) {
	if tenantID == "" {
		return nil, errors.New("endpoint: empty tenant_id")
	}
	if id == "" {
		return nil, errors.New("endpoint: empty id")
	}
	var ep Endpoint
	err := r.db.GetContext(ctx, &ep, r.db.Rebind(
		`SELECT `+epColumns+` FROM endpoints WHERE tenant_id = ? AND id = ?`),
		tenantID, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("endpoint: not found: tenant=%s id=%s", tenantID, id)
		}
		return nil, fmt.Errorf("endpoint: get by id: %w", err)
	}
	return &ep, nil
}

// List 实现 EndpointReader.List。
func (r *SQLEndpointReader) List(ctx context.Context, tenantID string) ([]*Endpoint, error) {
	if tenantID == "" {
		return nil, errors.New("endpoint: empty tenant_id")
	}
	var rows []Endpoint
	if err := r.db.SelectContext(ctx, &rows,
		r.db.Rebind(`SELECT `+epColumns+` FROM endpoints WHERE tenant_id = ? ORDER BY id`),
		tenantID,
	); err != nil {
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
