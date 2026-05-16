package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// SQLEndpointReader 是 EndpointReader 的 sqlx 实现。
//
// **v0.3 改动**：去 account_id（endpoints 是全局上游池；BYOK 等真要做时加 nullable account_id）。
type SQLEndpointReader struct {
	db *sqlx.DB
}

// NewSQLEndpointReader 用现成 *sqlx.DB 构造。
func NewSQLEndpointReader(db *sqlx.DB) *SQLEndpointReader {
	return &SQLEndpointReader{db: db}
}

const epColumns = `id, name, vendor, model, group_name, weight, enabled,
	auth, routing, quota, capabilities, extra,
	created_at, updated_at, deleted_at`

// ListForModel 实现 EndpointReader.ListForModel。
//
// 返回所有匹配 (model, group_name) 且 enabled / 未删 的 endpoints，按 weight DESC 排序，
// 同 weight 内按 id ASC（稳定）。M7 LimitReadFilter 顺序遍历，第一个未超 endpoint quota 的入选。
//
// 找不到 → (空切片, nil)；调用方自己 abort 503。
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

// PickForModel 实现 EndpointReader.PickForModel。
//
// v0.1：按 (model, group_name) + enabled = 1 + deleted_at IS NULL 取第一行。
// 无加权 / 无 Filter / 无 Cooldown（v0.5+ pkg/schedule 接管）。
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

// GetByID 实现 EndpointReader.GetByID。BIGINT id。
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

// List 实现 EndpointReader.List；列全部未删 endpoints。
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

// 编译期断言。
var _ EndpointReader = (*SQLEndpointReader)(nil)
