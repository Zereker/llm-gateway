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
// 多租户：所有 query 按 tenantID 范围内（v0.1 admin 单租户操作 "default"，
// 调用方可以传任意 tenantID 准备未来多租户）。
type ModelServiceStore struct {
	db *gorm.DB
}

// NewModelServiceStore 用现成 *gorm.DB 构造。
func NewModelServiceStore(db *gorm.DB) *ModelServiceStore {
	return &ModelServiceStore{db: db}
}

// GetByModel 按 (tenant, model) 查；找不到返回带 key 信息的错误。
func (s *ModelServiceStore) GetByModel(ctx context.Context, tenantID, model string) (*repo.ModelService, error) {
	if tenantID == "" || model == "" {
		return nil, errors.New("model_service: tenant_id and model required")
	}
	var ms repo.ModelService
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND model = ?", tenantID, model).
		First(&ms).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("model_service: not found: tenant=%s model=%s", tenantID, model)
		}
		return nil, fmt.Errorf("model_service: get by model: %w", err)
	}
	return &ms, nil
}

// List 列租户范围内的全部记录。
func (s *ModelServiceStore) List(ctx context.Context, tenantID string) ([]repo.ModelService, error) {
	if tenantID == "" {
		return nil, errors.New("model_service: tenant_id required")
	}
	var out []repo.ModelService
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Order("id ASC").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("model_service: list: %w", err)
	}
	return out, nil
}

// Create 插入新记录；成功后回填 m.ID + m.UpdateTime。tenant_id 默认 "default"。
func (s *ModelServiceStore) Create(ctx context.Context, m *repo.ModelService) error {
	if m == nil || m.Model == "" || m.ServiceID == "" {
		return errors.New("model_service: model and service_id required")
	}
	if m.TenantID == "" {
		m.TenantID = "default"
	}
	if m.UpdateTime.IsZero() {
		m.UpdateTime = time.Now().UTC()
	}
	if m.Group == "" {
		m.Group = "default"
	}
	if err := s.db.WithContext(ctx).Create(m).Error; err != nil {
		return fmt.Errorf("model_service: create: %w", err)
	}
	return nil
}

// Update 按 (tenant_id, model) 定位行，更新除 id / model / tenant_id 外所有字段。
func (s *ModelServiceStore) Update(ctx context.Context, m *repo.ModelService) error {
	if m == nil || m.Model == "" {
		return errors.New("model_service: model required for update")
	}
	if m.TenantID == "" {
		m.TenantID = "default"
	}
	if m.Group == "" {
		m.Group = "default"
	}
	m.UpdateTime = time.Now().UTC()

	res := s.db.WithContext(ctx).
		Model(&repo.ModelService{}).
		Where("tenant_id = ? AND model = ?", m.TenantID, m.Model).
		Updates(map[string]any{
			"service_id":  m.ServiceID,
			"update_time": m.UpdateTime,
			"spec_detail": m.SpecDetail,
			"group_name":  m.Group,
			"tpm":         m.Tpm,
			"rpm":         m.Rpm,
		})
	if res.Error != nil {
		return fmt.Errorf("model_service: update: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("model_service: not found: tenant=%s model=%s", m.TenantID, m.Model)
	}
	return nil
}

func (s *ModelServiceStore) Delete(ctx context.Context, tenantID, model string) error {
	if tenantID == "" || model == "" {
		return errors.New("model_service: tenant_id and model required")
	}
	res := s.db.WithContext(ctx).
		Where("tenant_id = ? AND model = ?", tenantID, model).
		Delete(&repo.ModelService{})
	if res.Error != nil {
		return fmt.Errorf("model_service: delete: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("model_service: not found: tenant=%s model=%s", tenantID, model)
	}
	return nil
}
