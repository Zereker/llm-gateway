package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// SQLModelServiceReader 是 ModelServiceReader 的 sqlx 实现。
//
// 字段映射 ModelService（pkg/repo/models.go）的 db: tag；JSON 列由
// datatypes.JSON 自带的 Scanner 接管。
type SQLModelServiceReader struct {
	db *sqlx.DB
}

// NewSQLModelServiceReader 用现成的 *sqlx.DB 构造（不打开新连接）。
func NewSQLModelServiceReader(db *sqlx.DB) *SQLModelServiceReader {
	return &SQLModelServiceReader{db: db}
}

const msColumns = `id, tenant_id, service_id, model, update_time, spec_detail, group_name, tpm, rpm`

// GetByModel 实现 ModelServiceReader.GetByModel；按 (tenant_id, model) 查。
func (r *SQLModelServiceReader) GetByModel(ctx context.Context, tenantID, model string) (*ModelService, error) {
	if tenantID == "" {
		return nil, errors.New("model_service: empty tenant_id")
	}
	if model == "" {
		return nil, errors.New("model_service: empty model name")
	}
	var ms ModelService
	err := r.db.GetContext(ctx, &ms, r.db.Rebind(
		`SELECT `+msColumns+` FROM model_services WHERE tenant_id = ? AND model = ?`),
		tenantID, model)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("model_service: not found: tenant=%s model=%s", tenantID, model)
		}
		return nil, fmt.Errorf("model_service: get by model: %w", err)
	}
	return &ms, nil
}

// List 实现 ModelServiceReader.List；只列指定 tenant 的。
func (r *SQLModelServiceReader) List(ctx context.Context, tenantID string) ([]*ModelService, error) {
	if tenantID == "" {
		return nil, errors.New("model_service: empty tenant_id")
	}
	var rows []ModelService
	if err := r.db.SelectContext(ctx, &rows,
		r.db.Rebind(`SELECT `+msColumns+` FROM model_services WHERE tenant_id = ? ORDER BY id`),
		tenantID,
	); err != nil {
		return nil, fmt.Errorf("model_service: list: %w", err)
	}
	out := make([]*ModelService, len(rows))
	for i := range rows {
		out[i] = &rows[i]
	}
	return out, nil
}

// 编译期断言。
var _ ModelServiceReader = (*SQLModelServiceReader)(nil)
