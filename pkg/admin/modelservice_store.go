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
// 没拆 interface——只有这一个实现，handlers 直接拿 *ModelServiceStore 用就行；
// 加 interface 是过度设计（不会有第二实现，不需要 stub 测试，admin tests 直接连真 MySQL）。
//
// 实体类型用 repo.ModelService（pkg/repo 持有，gateway 也共享）。
type ModelServiceStore struct {
	db *gorm.DB
}

// NewModelServiceStore 用现成 *gorm.DB 构造。
func NewModelServiceStore(db *gorm.DB) *ModelServiceStore {
	return &ModelServiceStore{db: db}
}

// GetByModel 按 model 字段查；找不到返回带表 / key 信息的错误。
func (s *ModelServiceStore) GetByModel(ctx context.Context, model string) (*repo.ModelService, error) {
	if model == "" {
		return nil, errors.New("model_service: empty model name")
	}
	var ms repo.ModelService
	if err := s.db.WithContext(ctx).Where("model = ?", model).First(&ms).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("model_service: not found: %s", model)
		}
		return nil, fmt.Errorf("model_service: get by model: %w", err)
	}
	return &ms, nil
}

// List 全量返回；admin 列表 UI 用。
func (s *ModelServiceStore) List(ctx context.Context) ([]repo.ModelService, error) {
	var out []repo.ModelService
	if err := s.db.WithContext(ctx).Order("id ASC").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("model_service: list: %w", err)
	}
	return out, nil
}

// Create 插入新记录；成功后回填 m.ID + m.UpdateTime。
func (s *ModelServiceStore) Create(ctx context.Context, m *repo.ModelService) error {
	if m == nil || m.Model == "" || m.ServiceID == "" {
		return errors.New("model_service: model and service_id required")
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

// Update 按 m.Model 字段定位行，更新除 id / model 外所有字段；服务端覆写 UpdateTime。
func (s *ModelServiceStore) Update(ctx context.Context, m *repo.ModelService) error {
	if m == nil || m.Model == "" {
		return errors.New("model_service: model required for update")
	}
	if m.Group == "" {
		m.Group = "default"
	}
	m.UpdateTime = time.Now().UTC()

	res := s.db.WithContext(ctx).
		Model(&repo.ModelService{}).
		Where("model = ?", m.Model).
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
		return fmt.Errorf("model_service: not found: %s", m.Model)
	}
	return nil
}

func (s *ModelServiceStore) Delete(ctx context.Context, model string) error {
	if model == "" {
		return errors.New("model_service: empty model name")
	}
	res := s.db.WithContext(ctx).Where("model = ?", model).Delete(&repo.ModelService{})
	if res.Error != nil {
		return fmt.Errorf("model_service: delete: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("model_service: not found: %s", model)
	}
	return nil
}
