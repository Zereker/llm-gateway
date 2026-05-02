package admin

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/zereker-labs/ai-gateway/pkg/repo"
)

// EndpointStore 用 gorm 写 endpoints 表。多租户 scope 同 ModelServiceStore。
type EndpointStore struct {
	db *gorm.DB
}

// NewEndpointStore 用现成 *gorm.DB 构造。
func NewEndpointStore(db *gorm.DB) *EndpointStore {
	return &EndpointStore{db: db}
}

func (s *EndpointStore) GetByID(ctx context.Context, tenantID, id string) (*repo.Endpoint, error) {
	if tenantID == "" || id == "" {
		return nil, errors.New("endpoint: tenant_id and id required")
	}
	var ep repo.Endpoint
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		First(&ep).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("endpoint: not found: tenant=%s id=%s", tenantID, id)
		}
		return nil, fmt.Errorf("endpoint: get by id: %w", err)
	}
	return &ep, nil
}

func (s *EndpointStore) List(ctx context.Context, tenantID string) ([]repo.Endpoint, error) {
	if tenantID == "" {
		return nil, errors.New("endpoint: tenant_id required")
	}
	var out []repo.Endpoint
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Order("id ASC").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("endpoint: list: %w", err)
	}
	return out, nil
}

func (s *EndpointStore) Create(ctx context.Context, e *repo.Endpoint) error {
	if e == nil || e.ID == "" || e.Vendor == "" || e.Model == "" {
		return errors.New("endpoint: id, vendor, model required")
	}
	if e.TenantID == "" {
		e.TenantID = "default"
	}
	if e.Group == "" {
		e.Group = "default"
	}
	if e.Weight == 0 {
		e.Weight = 100
	}
	if err := s.db.WithContext(ctx).Create(e).Error; err != nil {
		return fmt.Errorf("endpoint: create: %w", err)
	}
	return nil
}

// Update 按 (tenant_id, id) 定位行，更新除 id / tenant_id 外所有字段。
func (s *EndpointStore) Update(ctx context.Context, e *repo.Endpoint) error {
	if e == nil || e.ID == "" {
		return errors.New("endpoint: id required for update")
	}
	if e.TenantID == "" {
		e.TenantID = "default"
	}
	if e.Group == "" {
		e.Group = "default"
	}

	res := s.db.WithContext(ctx).
		Model(&repo.Endpoint{}).
		Where("tenant_id = ? AND id = ?", e.TenantID, e.ID).
		Updates(map[string]any{
			"vendor":       e.Vendor,
			"url":          e.URL,
			"api_key":      string(e.APIKey),
			"group_name":   e.Group,
			"model":        e.Model,
			"weight":       e.Weight,
			"rpm":          e.RPM,
			"tpm":          e.TPM,
			"rps":          e.RPS,
			"capabilities": e.Capabilities,
			"extra":        e.Extra,
		})
	if res.Error != nil {
		return fmt.Errorf("endpoint: update: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("endpoint: not found: tenant=%s id=%s", e.TenantID, e.ID)
	}
	return nil
}

func (s *EndpointStore) Delete(ctx context.Context, tenantID, id string) error {
	if tenantID == "" || id == "" {
		return errors.New("endpoint: tenant_id and id required")
	}
	res := s.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		Delete(&repo.Endpoint{})
	if res.Error != nil {
		return fmt.Errorf("endpoint: delete: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("endpoint: not found: tenant=%s id=%s", tenantID, id)
	}
	return nil
}
