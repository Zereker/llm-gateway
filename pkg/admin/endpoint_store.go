package admin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/zereker/llm-gateway/pkg/repo"
)

// EndpointStore 用 gorm 写 endpoints 表。
//
// **v0.3 改动**：去 account_id（全局上游池；BYOK 等真要做时再加 nullable account_id 列）。
type EndpointStore struct {
	db *gorm.DB
}

// NewEndpointStore 用现成 *gorm.DB 构造。
func NewEndpointStore(db *gorm.DB) *EndpointStore {
	return &EndpointStore{db: db}
}

// GetByID 按 id 查未删 endpoint。
func (s *EndpointStore) GetByID(ctx context.Context, id int64) (*repo.Endpoint, error) {
	if id == 0 {
		return nil, errors.New("endpoint: id required")
	}
	var ep repo.Endpoint
	if err := s.db.WithContext(ctx).
		Where("id = ? AND deleted_at IS NULL", id).
		First(&ep).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("endpoint: not found: id=%d", id)
		}
		return nil, fmt.Errorf("endpoint: get by id: %w", err)
	}
	return &ep, nil
}

// GetByName 按 name 查；admin UI 用名字找 ID。
func (s *EndpointStore) GetByName(ctx context.Context, name string) (*repo.Endpoint, error) {
	if name == "" {
		return nil, errors.New("endpoint: name required")
	}
	var ep repo.Endpoint
	if err := s.db.WithContext(ctx).
		Where("name = ? AND deleted_at IS NULL", name).
		First(&ep).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("endpoint: not found: name=%s", name)
		}
		return nil, fmt.Errorf("endpoint: get by name: %w", err)
	}
	return &ep, nil
}

// List 列全部未删 endpoint。
func (s *EndpointStore) List(ctx context.Context) ([]repo.Endpoint, error) {
	var out []repo.Endpoint
	if err := s.db.WithContext(ctx).
		Where("deleted_at IS NULL").
		Order("id ASC").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("endpoint: list: %w", err)
	}
	return out, nil
}

// Create 插入新 endpoint；成功后回填 e.ID。
//
// 必填：name / vendor / protocol / model / Auth.Type / Routing。
func (s *EndpointStore) Create(ctx context.Context, e *repo.Endpoint) error {
	if e == nil || e.Name == "" || e.Vendor == "" || e.Model == "" {
		return errors.New("endpoint: name, vendor, model required")
	}
	if e.Protocol == "" || e.Protocol == "unknown" {
		return errors.New("endpoint: protocol required (openai|anthropic|gemini|responses|...)")
	}
	if e.Auth.Type == "" {
		return errors.New("endpoint: auth.type required")
	}
	if (e.Routing == repo.RoutingConfig{}) {
		return errors.New("endpoint: routing required (at least URL or region)")
	}
	if err := validateRoutingURL(&e.Routing); err != nil {
		return fmt.Errorf("endpoint: %w", err)
	}
	if e.Group == "" {
		e.Group = "default"
	}
	if e.Weight == 0 {
		e.Weight = 100
	}
	e.Enabled = true
	now := time.Now().UTC()
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now
	}
	if e.UpdatedAt.IsZero() {
		e.UpdatedAt = now
	}
	if err := s.db.WithContext(ctx).Create(e).Error; err != nil {
		return fmt.Errorf("endpoint: create: %w", err)
	}
	return nil
}

// Update 按 id 定位行，更新除 id / name 外所有字段。
func (s *EndpointStore) Update(ctx context.Context, e *repo.Endpoint) error {
	if e == nil || e.ID == 0 {
		return errors.New("endpoint: id required for update")
	}
	if err := validateRoutingURL(&e.Routing); err != nil {
		return fmt.Errorf("endpoint: %w", err)
	}
	if e.Group == "" {
		e.Group = "default"
	}

	res := s.db.WithContext(ctx).
		Model(&repo.Endpoint{}).
		Where("id = ? AND deleted_at IS NULL", e.ID).
		Updates(map[string]any{
			"vendor":       e.Vendor,
			"protocol":     e.Protocol,
			"model":        e.Model,
			"group_name":   e.Group,
			"weight":       e.Weight,
			"enabled":      e.Enabled,
			"auth":         e.Auth,
			"routing":      e.Routing,
			"quota":        e.Quota,
			"capabilities": e.Capabilities,
			"extra":        e.Extra,
		})
	if res.Error != nil {
		return fmt.Errorf("endpoint: update: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("endpoint: not found: id=%d", e.ID)
	}
	return nil
}

// Delete 软删 set deleted_at = NOW()。
func (s *EndpointStore) Delete(ctx context.Context, id int64) error {
	if id == 0 {
		return errors.New("endpoint: id required")
	}
	now := time.Now().UTC()
	res := s.db.WithContext(ctx).
		Model(&repo.Endpoint{}).
		Where("id = ? AND deleted_at IS NULL", id).
		Updates(map[string]any{"deleted_at": now})
	if res.Error != nil {
		return fmt.Errorf("endpoint: delete: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("endpoint: not found: id=%d", id)
	}
	return nil
}
