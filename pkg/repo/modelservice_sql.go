package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// SQLModelServiceRepo 是 ModelServiceReader + Writer 的 sqlx 实现。
//
// 字段映射 dbModelService ↔ domain.ModelServiceSnapshot；JSON 字段
// （SpecDetail）以 TEXT 形态存储，读出时还原为 json.RawMessage。
type SQLModelServiceRepo struct {
	db *sqlx.DB
}

// NewSQLModelServiceRepo 用现成的 *sqlx.DB 构造。
//
// 不在本包打开连接：DB 生命周期归 main，pkg/repo 只是用户。
func NewSQLModelServiceRepo(db *sqlx.DB) *SQLModelServiceRepo {
	return &SQLModelServiceRepo{db: db}
}

// dbModelService 是行级表示，column 名跟 schema.sql 对齐。
type dbModelService struct {
	ID         int64     `db:"id"`
	ServiceID  string    `db:"service_id"`
	Model      string    `db:"model"`
	UpdateTime time.Time `db:"update_time"`
	SpecDetail string    `db:"spec_detail"`
	GroupName  string    `db:"group_name"`
	Tpm        int64     `db:"tpm"`
	Rpm        int64     `db:"rpm"`
}

const msColumns = `id, service_id, model, update_time, spec_detail, group_name, tpm, rpm`

// GetByModel 实现 ModelServiceReader.GetByModel。
func (r *SQLModelServiceRepo) GetByModel(ctx context.Context, model string) (*domain.ModelServiceSnapshot, error) {
	if model == "" {
		return nil, errors.New("model_service: empty model name")
	}
	var row dbModelService
	err := r.db.GetContext(ctx, &row, r.db.Rebind(
		`SELECT `+msColumns+` FROM model_services WHERE model = ?`), model)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("model_service: not found: %s", model)
		}
		return nil, fmt.Errorf("model_service: get by model: %w", err)
	}
	return rowToModelService(row), nil
}

// List 实现 ModelServiceReader.List。
func (r *SQLModelServiceRepo) List(ctx context.Context) ([]*domain.ModelServiceSnapshot, error) {
	var rows []dbModelService
	if err := r.db.SelectContext(ctx, &rows,
		`SELECT `+msColumns+` FROM model_services ORDER BY id`); err != nil {
		return nil, fmt.Errorf("model_service: list: %w", err)
	}
	out := make([]*domain.ModelServiceSnapshot, len(rows))
	for i := range rows {
		out[i] = rowToModelService(rows[i])
	}
	return out, nil
}

// Create 实现 ModelServiceWriter.Create；成功后回填 snap.ID。
func (r *SQLModelServiceRepo) Create(ctx context.Context, snap *domain.ModelServiceSnapshot) error {
	if snap == nil || snap.Model == "" || snap.ServiceID == "" {
		return errors.New("model_service: model and service_id required")
	}
	if snap.UpdateTime.IsZero() {
		snap.UpdateTime = time.Now().UTC()
	}
	groupName := snap.Group
	if groupName == "" {
		groupName = "default"
	}
	res, err := r.db.ExecContext(ctx, r.db.Rebind(
		`INSERT INTO model_services (service_id, model, update_time, spec_detail, group_name, tpm, rpm)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`),
		snap.ServiceID, snap.Model, snap.UpdateTime, string(snap.SpecDetail),
		groupName, snap.Tpm, snap.Rpm,
	)
	if err != nil {
		return fmt.Errorf("model_service: create: %w", err)
	}
	if id, err := res.LastInsertId(); err == nil {
		snap.ID = id
	}
	return nil
}

// Update 实现 ModelServiceWriter.Update。
//
// 按 model 字段查找；UpdateTime 服务端覆写为 now（admin 不需要传）。
func (r *SQLModelServiceRepo) Update(ctx context.Context, snap *domain.ModelServiceSnapshot) error {
	if snap == nil || snap.Model == "" {
		return errors.New("model_service: model required for update")
	}
	snap.UpdateTime = time.Now().UTC()
	groupName := snap.Group
	if groupName == "" {
		groupName = "default"
	}
	res, err := r.db.ExecContext(ctx, r.db.Rebind(
		`UPDATE model_services
		 SET service_id = ?, update_time = ?, spec_detail = ?, group_name = ?, tpm = ?, rpm = ?
		 WHERE model = ?`),
		snap.ServiceID, snap.UpdateTime, string(snap.SpecDetail),
		groupName, snap.Tpm, snap.Rpm, snap.Model,
	)
	if err != nil {
		return fmt.Errorf("model_service: update: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("model_service: not found: %s", snap.Model)
	}
	return nil
}

// Delete 实现 ModelServiceWriter.Delete。
func (r *SQLModelServiceRepo) Delete(ctx context.Context, model string) error {
	if model == "" {
		return errors.New("model_service: empty model name")
	}
	res, err := r.db.ExecContext(ctx,
		r.db.Rebind(`DELETE FROM model_services WHERE model = ?`), model)
	if err != nil {
		return fmt.Errorf("model_service: delete: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("model_service: not found: %s", model)
	}
	return nil
}

func rowToModelService(row dbModelService) *domain.ModelServiceSnapshot {
	snap := &domain.ModelServiceSnapshot{
		ID:         row.ID,
		ServiceID:  row.ServiceID,
		Model:      row.Model,
		UpdateTime: row.UpdateTime,
		Group:      row.GroupName,
		Tpm:        row.Tpm,
		Rpm:        row.Rpm,
	}
	if row.SpecDetail != "" {
		snap.SpecDetail = json.RawMessage(row.SpecDetail)
	}
	return snap
}

// 编译期断言：SQL 实现同时满足 Reader + Writer。
var (
	_ ModelServiceReader     = (*SQLModelServiceRepo)(nil)
	_ ModelServiceWriter     = (*SQLModelServiceRepo)(nil)
	_ ModelServiceRepository = (*SQLModelServiceRepo)(nil)
)
