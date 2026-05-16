package admin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/zereker/llm-gateway/pkg/repo"
)

// SubscriptionStore 用 gorm 写 account_model_subscriptions 表。
//
// 业务语义："account pin 订阅了哪些模型"。M5 在路上查这张表决定 200/403。
//
// admin 接口都用 (account_pin, model_service_id) 复合键操作；不暴露 subscription
// 的内部 BIGINT id。
type SubscriptionStore struct {
	db *gorm.DB
}

func NewSubscriptionStore(db *gorm.DB) *SubscriptionStore {
	return &SubscriptionStore{db: db}
}

// ListByAccount 列某 account 已订阅的全部模型（含 enabled = false / deleted = NULL 的）。
//
// admin UI 用：显示 account 的"订阅清单"。
func (s *SubscriptionStore) ListByAccount(ctx context.Context, accountID string) ([]repo.AccountModelSubscription, error) {
	if accountID == "" {
		return nil, errors.New("subscription: account_id required")
	}
	var out []repo.AccountModelSubscription
	if err := s.db.WithContext(ctx).
		Where("account_id = ? AND deleted_at IS NULL", accountID).
		Order("id ASC").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("subscription: list: %w", err)
	}
	return out, nil
}

// Subscribe 创建订阅；如果 (account, model_service) 已存在但被软删，restore 它（清 deleted_at + 启用）。
// 重复 subscribe 同一对组合返回 ErrAlreadySubscribed（admin 应该走 SetEnabled）。
func (s *SubscriptionStore) Subscribe(ctx context.Context, accountID string, modelServiceID int64) (*repo.AccountModelSubscription, error) {
	if accountID == "" || modelServiceID == 0 {
		return nil, errors.New("subscription: account_id and model_service_id required")
	}

	// 先看是否已有行（含软删）
	var existing repo.AccountModelSubscription
	err := s.db.WithContext(ctx).
		Where("account_id = ? AND model_service_id = ?", accountID, modelServiceID).
		First(&existing).Error
	if err == nil {
		if existing.DeletedAt == nil && existing.Enabled {
			return nil, ErrAlreadySubscribed
		}
		// restore：清 deleted_at + enable
		now := time.Now().UTC()
		if err := s.db.WithContext(ctx).Model(&existing).Updates(map[string]any{
			"deleted_at": nil,
			"enabled":    true,
			"updated_at": now,
		}).Error; err != nil {
			return nil, fmt.Errorf("subscription: restore: %w", err)
		}
		existing.DeletedAt = nil
		existing.Enabled = true
		existing.UpdatedAt = now
		return &existing, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("subscription: lookup before create: %w", err)
	}

	// 全新订阅
	now := time.Now().UTC()
	row := &repo.AccountModelSubscription{
		AccountID:      accountID,
		ModelServiceID: modelServiceID,
		Enabled:        true,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
		return nil, fmt.Errorf("subscription: create: %w", err)
	}
	return row, nil
}

// SetEnabled 切换 enabled 状态（不删行）。
func (s *SubscriptionStore) SetEnabled(ctx context.Context, accountID string, modelServiceID int64, enabled bool) error {
	if accountID == "" || modelServiceID == 0 {
		return errors.New("subscription: account_id and model_service_id required")
	}
	res := s.db.WithContext(ctx).
		Model(&repo.AccountModelSubscription{}).
		Where("account_id = ? AND model_service_id = ? AND deleted_at IS NULL", accountID, modelServiceID).
		Updates(map[string]any{"enabled": enabled})
	if res.Error != nil {
		return fmt.Errorf("subscription: set enabled: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("subscription: not found: account=%s model_service_id=%d", accountID, modelServiceID)
	}
	return nil
}

// Unsubscribe 软删订阅 set deleted_at = NOW()。
func (s *SubscriptionStore) Unsubscribe(ctx context.Context, accountID string, modelServiceID int64) error {
	if accountID == "" || modelServiceID == 0 {
		return errors.New("subscription: account_id and model_service_id required")
	}
	now := time.Now().UTC()
	res := s.db.WithContext(ctx).
		Model(&repo.AccountModelSubscription{}).
		Where("account_id = ? AND model_service_id = ? AND deleted_at IS NULL", accountID, modelServiceID).
		Updates(map[string]any{"deleted_at": now, "enabled": false})
	if res.Error != nil {
		return fmt.Errorf("subscription: unsubscribe: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("subscription: not found: account=%s model_service_id=%d", accountID, modelServiceID)
	}
	return nil
}

// ErrAlreadySubscribed POST /subscriptions 重复订阅时返回；admin handler 转 409。
var ErrAlreadySubscribed = errors.New("subscription: already subscribed")
