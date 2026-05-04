package admin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/zereker-labs/ai-gateway/pkg/repo"
)

// TenantStore 用 gorm 写 tenants 表（业务线 / pin 元信息 + quota_policy 引用）。
//
// pin 是 PK；其他表的 tenant_id 字段 FK → tenants.pin。所以建 tenant 必须先于
// 创建该 tenant 下的 apikey / pricing / subscription。
type TenantStore struct {
	db *gorm.DB
}

func NewTenantStore(db *gorm.DB) *TenantStore {
	return &TenantStore{db: db}
}

// GetByPin 按 pin 查未删 tenant。
func (s *TenantStore) GetByPin(ctx context.Context, pin string) (*repo.Tenant, error) {
	if pin == "" {
		return nil, errors.New("tenant: pin required")
	}
	var t repo.Tenant
	if err := s.db.WithContext(ctx).
		Where("pin = ? AND deleted_at IS NULL", pin).
		First(&t).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("tenant: not found: pin=%s", pin)
		}
		return nil, fmt.Errorf("tenant: get by pin: %w", err)
	}
	return &t, nil
}

// List 列全部未删 tenants。
func (s *TenantStore) List(ctx context.Context) ([]repo.Tenant, error) {
	var out []repo.Tenant
	if err := s.db.WithContext(ctx).
		Where("deleted_at IS NULL").
		Order("pin ASC").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("tenant: list: %w", err)
	}
	return out, nil
}

// Create 插入新 tenant。pin 必须 unique。
func (s *TenantStore) Create(ctx context.Context, t *repo.Tenant) error {
	if t == nil || t.Pin == "" || t.Name == "" {
		return errors.New("tenant: pin and name required")
	}
	t.Enabled = true
	now := time.Now().UTC()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = now
	}
	if err := s.db.WithContext(ctx).Create(t).Error; err != nil {
		return fmt.Errorf("tenant: create: %w", err)
	}
	return nil
}

// Update 更新可变字段：name / enabled / quota_policy_id。pin 不可改（业务键 + FK 锚点）。
func (s *TenantStore) Update(ctx context.Context, pin string, updates TenantUpdates) error {
	if pin == "" {
		return errors.New("tenant: pin required")
	}
	patch := map[string]any{}
	if updates.Name != nil {
		patch["name"] = *updates.Name
	}
	if updates.Enabled != nil {
		patch["enabled"] = *updates.Enabled
	}
	if updates.QuotaPolicyID != nil {
		patch["quota_policy_id"] = *updates.QuotaPolicyID
	}
	if updates.ClearQuotaPolicy {
		patch["quota_policy_id"] = nil
	}
	if len(patch) == 0 {
		return errors.New("tenant: no updatable fields supplied")
	}
	res := s.db.WithContext(ctx).
		Model(&repo.Tenant{}).
		Where("pin = ? AND deleted_at IS NULL", pin).
		Updates(patch)
	if res.Error != nil {
		return fmt.Errorf("tenant: update: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("tenant: not found: pin=%s", pin)
	}
	return nil
}

// TenantUpdates 是 Update 可改字段。
type TenantUpdates struct {
	Name             *string
	Enabled          *bool
	QuotaPolicyID    *int64
	ClearQuotaPolicy bool
}

// Delete 软删 set deleted_at = NOW()。
//
// **注意**：tenant 软删后该 pin 仍占 PK；同 pin 不能直接重建（FK 也仍指向）。
// 真要复用 pin 必须 hard-delete + 清理所有引用记录。
func (s *TenantStore) Delete(ctx context.Context, pin string) error {
	if pin == "" {
		return errors.New("tenant: pin required")
	}
	now := time.Now().UTC()
	res := s.db.WithContext(ctx).
		Model(&repo.Tenant{}).
		Where("pin = ? AND deleted_at IS NULL", pin).
		Updates(map[string]any{"deleted_at": now})
	if res.Error != nil {
		return fmt.Errorf("tenant: delete: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("tenant: not found: pin=%s", pin)
	}
	return nil
}
