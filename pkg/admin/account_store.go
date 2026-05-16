package admin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/zereker/llm-gateway/pkg/repo"
)

// AccountStore 用 gorm 写 accounts 表（业务线 / pin 元信息 + quota_policy 引用）。
//
// pin 是 PK；其他表的 account_id 字段 FK → accounts.pin。所以建 account 必须先于
// 创建该 account 下的 apikey / pricing / subscription。
type AccountStore struct {
	db *gorm.DB
}

func NewAccountStore(db *gorm.DB) *AccountStore {
	return &AccountStore{db: db}
}

// GetByPin 按 pin 查未删 account。
func (s *AccountStore) GetByPin(ctx context.Context, pin string) (*repo.Account, error) {
	if pin == "" {
		return nil, errors.New("account: pin required")
	}
	var t repo.Account
	if err := s.db.WithContext(ctx).
		Where("pin = ? AND deleted_at IS NULL", pin).
		First(&t).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("account: not found: pin=%s", pin)
		}
		return nil, fmt.Errorf("account: get by pin: %w", err)
	}
	return &t, nil
}

// List 列全部未删 accounts。
func (s *AccountStore) List(ctx context.Context) ([]repo.Account, error) {
	var out []repo.Account
	if err := s.db.WithContext(ctx).
		Where("deleted_at IS NULL").
		Order("pin ASC").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("account: list: %w", err)
	}
	return out, nil
}

// Create 插入新 account。pin 必须 unique。
func (s *AccountStore) Create(ctx context.Context, t *repo.Account) error {
	if t == nil || t.Pin == "" || t.Name == "" {
		return errors.New("account: pin and name required")
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
		return fmt.Errorf("account: create: %w", err)
	}
	return nil
}

// Update 更新可变字段：name / enabled / quota_policy_id。pin 不可改（业务键 + FK 锚点）。
func (s *AccountStore) Update(ctx context.Context, pin string, updates AccountUpdates) error {
	if pin == "" {
		return errors.New("account: pin required")
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
		return errors.New("account: no updatable fields supplied")
	}
	res := s.db.WithContext(ctx).
		Model(&repo.Account{}).
		Where("pin = ? AND deleted_at IS NULL", pin).
		Updates(patch)
	if res.Error != nil {
		return fmt.Errorf("account: update: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("account: not found: pin=%s", pin)
	}
	return nil
}

// AccountUpdates 是 Update 可改字段。
type AccountUpdates struct {
	Name             *string
	Enabled          *bool
	QuotaPolicyID    *int64
	ClearQuotaPolicy bool
}

// Delete 软删 set deleted_at = NOW()。
//
// **注意**： account 软删后该 pin 仍占 PK；同 pin 不能直接重建（FK 也仍指向）。
// 真要复用 pin 必须 hard-delete + 清理所有引用记录。
func (s *AccountStore) Delete(ctx context.Context, pin string) error {
	if pin == "" {
		return errors.New("account: pin required")
	}
	now := time.Now().UTC()
	res := s.db.WithContext(ctx).
		Model(&repo.Account{}).
		Where("pin = ? AND deleted_at IS NULL", pin).
		Updates(map[string]any{"deleted_at": now})
	if res.Error != nil {
		return fmt.Errorf("account: delete: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("account: not found: pin=%s", pin)
	}
	return nil
}
