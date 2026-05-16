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
// **v0.3 改动**：去 account_id（model_services 是全局 catalog）；GetByModel 签名去 accountID 参数。
type SQLModelServiceReader struct {
	db *sqlx.DB
}

// NewSQLModelServiceReader 用现成的 *sqlx.DB 构造（不打开新连接）。
func NewSQLModelServiceReader(db *sqlx.DB) *SQLModelServiceReader {
	return &SQLModelServiceReader{db: db}
}

const msColumns = `id, service_id, model, created_at, updated_at, deleted_at`

// GetByModel 实现 ModelServiceReader.GetByModel；按 model 全局查。
func (r *SQLModelServiceReader) GetByModel(ctx context.Context, model string) (*ModelService, error) {
	if model == "" {
		return nil, errors.New("model_service: empty model name")
	}
	var ms ModelService
	err := r.db.GetContext(ctx, &ms, r.db.Rebind(
		`SELECT `+msColumns+` FROM model_services
		 WHERE model = ? AND deleted_at IS NULL`),
		model)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("model_service: not found: model=%s", model)
		}
		return nil, fmt.Errorf("model_service: get by model: %w", err)
	}
	return &ms, nil
}

// List 实现 ModelServiceReader.List；列全部未删 records（全局 catalog）。
func (r *SQLModelServiceReader) List(ctx context.Context) ([]*ModelService, error) {
	var rows []ModelService
	if err := r.db.SelectContext(ctx, &rows,
		`SELECT `+msColumns+` FROM model_services
		 WHERE deleted_at IS NULL ORDER BY id`,
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
