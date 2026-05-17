package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"gorm.io/gorm"

	"github.com/zereker/llm-gateway/pkg/cdcoutbox"
	"github.com/zereker/llm-gateway/pkg/repo"
)

// ModelServiceStore 用 gorm 写 model_services 表。
//
// **CDC 集成**：所有写操作（Create/Update/Delete）在事务里追加一行
// data_change_outbox，让 OutboxRelay 异步同步到 Redis（docs/06 设计）。
type ModelServiceStore struct {
	db     *gorm.DB
	outbox bool // true = 在 tx 内写 outbox；false = 跳过（dev / 单元测试）
}

// NewModelServiceStore 不带 outbox。
func NewModelServiceStore(db *gorm.DB) *ModelServiceStore {
	return &ModelServiceStore{db: db}
}

// NewModelServiceStoreWithOutbox 启用 outbox：所有写操作同事务追加 data_change_outbox 行。
func NewModelServiceStoreWithOutbox(db *gorm.DB) *ModelServiceStore {
	return &ModelServiceStore{db: db, outbox: true}
}

// GetByModel 按 model 查未删 record。
func (s *ModelServiceStore) GetByModel(ctx context.Context, model string) (*repo.ModelService, error) {
	if model == "" {
		return nil, errors.New("model_service: model required")
	}
	var ms repo.ModelService
	if err := s.db.WithContext(ctx).
		Where("model = ? AND deleted_at IS NULL", model).
		First(&ms).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("model_service: not found: model=%s", model)
		}
		return nil, fmt.Errorf("model_service: get by model: %w", err)
	}
	return &ms, nil
}

// GetByID admin UI 按 ID 编辑详情。
func (s *ModelServiceStore) GetByID(ctx context.Context, id int64) (*repo.ModelService, error) {
	if id == 0 {
		return nil, errors.New("model_service: id required")
	}
	var ms repo.ModelService
	if err := s.db.WithContext(ctx).
		Where("id = ? AND deleted_at IS NULL", id).
		First(&ms).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("model_service: not found: id=%d", id)
		}
		return nil, fmt.Errorf("model_service: get by id: %w", err)
	}
	return &ms, nil
}

// List 列全部未删 records（全局 catalog）。
func (s *ModelServiceStore) List(ctx context.Context) ([]repo.ModelService, error) {
	var out []repo.ModelService
	if err := s.db.WithContext(ctx).
		Where("deleted_at IS NULL").
		Order("id ASC").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("model_service: list: %w", err)
	}
	return out, nil
}

// Create 插入新 model；成功后回填 m.ID + 三件套时间戳。
func (s *ModelServiceStore) Create(ctx context.Context, m *repo.ModelService) error {
	if m == nil || m.Model == "" || m.ServiceID == "" {
		return errors.New("model_service: model and service_id required")
	}
	now := time.Now().UTC()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = now
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(m).Error; err != nil {
			return fmt.Errorf("model_service: create: %w", err)
		}
		return s.appendOutbox(ctx, tx, cdcoutbox.OpUpsert, m)
	})
}

// Update 按 (model) 定位行，仅更新 service_id。
func (s *ModelServiceStore) Update(ctx context.Context, m *repo.ModelService) error {
	if m == nil || m.Model == "" {
		return errors.New("model_service: model required for update")
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&repo.ModelService{}).
			Where("model = ? AND deleted_at IS NULL", m.Model).
			Updates(map[string]any{"service_id": m.ServiceID})
		if res.Error != nil {
			return fmt.Errorf("model_service: update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("model_service: not found: model=%s", m.Model)
		}
		// 重读拿 ID 用于 outbox PK
		var fresh repo.ModelService
		if err := tx.Where("model = ? AND deleted_at IS NULL", m.Model).First(&fresh).Error; err != nil {
			return err
		}
		*m = fresh
		return s.appendOutbox(ctx, tx, cdcoutbox.OpUpsert, m)
	})
}

// Delete 软删 set deleted_at = NOW()。
func (s *ModelServiceStore) Delete(ctx context.Context, model string) error {
	if model == "" {
		return errors.New("model_service: model required")
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing repo.ModelService
		if err := tx.Where("model = ? AND deleted_at IS NULL", model).First(&existing).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("model_service: not found: model=%s", model)
			}
			return err
		}
		now := time.Now().UTC()
		if err := tx.Model(&existing).Update("deleted_at", now).Error; err != nil {
			return fmt.Errorf("model_service: delete: %w", err)
		}
		// outbox 用 delete op + model 作 pk（让 gateway 也能定位）
		return s.appendOutboxDelete(ctx, tx, &existing)
	})
}

// =============================================================================
// outbox 集成
// =============================================================================

// appendOutbox 在事务里写一行 data_change_outbox（upsert）。
//
// store.outbox=false 时跳过（兼容老 admin 装配 / 单元测试）。
func (s *ModelServiceStore) appendOutbox(ctx context.Context, tx *gorm.DB, op cdcoutbox.Op, m *repo.ModelService) error {
	if !s.outbox {
		return nil
	}
	payload, err := json.Marshal(domainModelService(m))
	if err != nil {
		return fmt.Errorf("model_service: outbox marshal: %w", err)
	}
	return cdcoutbox.AppendTx(ctx, gormExecer{tx: tx}, cdcoutbox.Change{
		Table:   "model_services",
		Op:      op,
		PK:      m.Model, // 用 model 名做 PK（与 ModelCatalog.GetByModel 的入参对齐）
		Payload: payload,
	})
}

// appendOutboxDelete 删除版本。
func (s *ModelServiceStore) appendOutboxDelete(ctx context.Context, tx *gorm.DB, m *repo.ModelService) error {
	if !s.outbox {
		return nil
	}
	return cdcoutbox.AppendTx(ctx, gormExecer{tx: tx}, cdcoutbox.Change{
		Table: "model_services",
		Op:    cdcoutbox.OpDelete,
		PK:    m.Model,
	})
}

// gormExecer 把 gorm.DB 适配为 cdcoutbox.TxExecer（返 sql.Result）。
type gormExecer struct{ tx *gorm.DB }

func (g gormExecer) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	res := g.tx.WithContext(ctx).Exec(query, args...)
	return sqlExecResult{rowsAffected: res.RowsAffected}, res.Error
}

// sqlExecResult 满足 sql.Result。
type sqlExecResult struct{ rowsAffected int64 }

func (r sqlExecResult) LastInsertId() (int64, error) { return 0, nil }
func (r sqlExecResult) RowsAffected() (int64, error) { return r.rowsAffected, nil }

// domainModelService 把 repo.ModelService → 跟 domain.ModelService 同 shape 的 map。
//
// 不直接 import pkg/domain（admin 不依赖 domain；保持现有 import 边界）；
// gateway 端反序列化用 domain.ModelService struct 反射。
type modelServicePayload struct {
	ID        int64      `json:"ID"`
	ServiceID string     `json:"ServiceID"`
	Model     string     `json:"Model"`
	CreatedAt time.Time  `json:"CreatedAt"`
	UpdatedAt time.Time  `json:"UpdatedAt"`
	DeletedAt *time.Time `json:"DeletedAt,omitempty"`
}

func domainModelService(m *repo.ModelService) modelServicePayload {
	return modelServicePayload{
		ID:        m.ID,
		ServiceID: m.ServiceID,
		Model:     m.Model,
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
		DeletedAt: m.DeletedAt,
	}
}

// 编译期断言：strconv 防 unused
var _ = strconv.Itoa
