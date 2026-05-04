package admin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/zereker-labs/ai-gateway/pkg/repo"
)

// ModelServiceStore 用 gorm 写 model_services 表。
//
// **v0.3 改动**：
//   - 全局 catalog（无 tenant_id）；模型可见性由 SubscriptionStore 管
//   - 删 tpm/rpm/group_name/spec_detail 字段
//   - 唯一键改 (service_id) + (model)
type ModelServiceStore struct {
	db *gorm.DB
}

func NewModelServiceStore(db *gorm.DB) *ModelServiceStore {
	return &ModelServiceStore{db: db}
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

// Create 插入新 model；成功后回填 m.ID + m.CreatedAt + m.UpdatedAt。
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
	if err := s.db.WithContext(ctx).Create(m).Error; err != nil {
		return fmt.Errorf("model_service: create: %w", err)
	}
	return nil
}

// Update 按 (model) 定位行，仅更新 service_id（model 是 UNIQUE 业务键，不可改）。
func (s *ModelServiceStore) Update(ctx context.Context, m *repo.ModelService) error {
	if m == nil || m.Model == "" {
		return errors.New("model_service: model required for update")
	}

	res := s.db.WithContext(ctx).
		Model(&repo.ModelService{}).
		Where("model = ? AND deleted_at IS NULL", m.Model).
		Updates(map[string]any{
			"service_id": m.ServiceID,
		})
	if res.Error != nil {
		return fmt.Errorf("model_service: update: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("model_service: not found: model=%s", m.Model)
	}
	return nil
}

// Delete 软删 set deleted_at = NOW()。
func (s *ModelServiceStore) Delete(ctx context.Context, model string) error {
	if model == "" {
		return errors.New("model_service: model required")
	}
	now := time.Now().UTC()
	res := s.db.WithContext(ctx).
		Model(&repo.ModelService{}).
		Where("model = ? AND deleted_at IS NULL", model).
		Updates(map[string]any{"deleted_at": now})
	if res.Error != nil {
		return fmt.Errorf("model_service: delete: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("model_service: not found: model=%s", model)
	}
	return nil
}
