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

const msColumns = `id, service_id, model, update_time, spec_detail, group_name, tpm, rpm`

// GetByModel 实现 ModelServiceReader.GetByModel。
func (r *SQLModelServiceReader) GetByModel(ctx context.Context, model string) (*ModelService, error) {
	if model == "" {
		return nil, errors.New("model_service: empty model name")
	}
	var ms ModelService
	err := r.db.GetContext(ctx, &ms, r.db.Rebind(
		`SELECT `+msColumns+` FROM model_services WHERE model = ?`), model)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("model_service: not found: %s", model)
		}
		return nil, fmt.Errorf("model_service: get by model: %w", err)
	}
	return &ms, nil
}

// List 实现 ModelServiceReader.List。
func (r *SQLModelServiceReader) List(ctx context.Context) ([]*ModelService, error) {
	var rows []ModelService
	if err := r.db.SelectContext(ctx, &rows,
		`SELECT `+msColumns+` FROM model_services ORDER BY id`); err != nil {
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
