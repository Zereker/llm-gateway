package admin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/zereker-labs/ai-gateway/pkg/repo"
)

// PricingStore 用 gorm 写 pricing_versions 表。
//
// **append-only 协议**：从不 UPDATE 已发布的 rule_json。改价 = RotatePrice
// 一个事务里两步：封盘旧 active + INSERT 新 active。
//
// 不暴露通用 Update / Delete API——开口子就破了 append-only invariant。
// 修笔误必须新发一版（notes 字段说明原因）。
type PricingStore struct {
	db *gorm.DB
}

func NewPricingStore(db *gorm.DB) *PricingStore {
	return &PricingStore{db: db}
}

// RotatePrice 发布新版本，原子地接替当前 active。
//
// 事务里：
//  1. UPDATE pricing_versions SET effective_to = NOW(6)
//     WHERE tenant_id=? AND model_service_id=? AND rule_class=? AND effective_to IS NULL
//  2. INSERT 新行 effective_from = NOW(6), effective_to = NULL
//
// 如果当前没 active（首次发布），步骤 1 影响 0 行也 OK；步骤 2 仍执行。
//
// 调用方拿到回填了 ID + EffectiveFrom 的新版本。
func (s *PricingStore) RotatePrice(
	ctx context.Context,
	tenantID string,
	modelServiceID int64,
	ruleClass string,
	ruleJSON datatypes.JSON,
	createdBy, notes string,
) (*repo.PricingVersion, error) {
	if tenantID == "" {
		tenantID = "default"
	}
	if modelServiceID == 0 {
		return nil, errors.New("pricing: model_service_id required")
	}
	if ruleClass == "" {
		ruleClass = "standard"
	}
	if len(ruleJSON) == 0 {
		return nil, errors.New("pricing: rule_json required")
	}

	now := time.Now().UTC()
	newRow := &repo.PricingVersion{
		TenantID:       tenantID,
		ModelServiceID: modelServiceID,
		RuleClass:      ruleClass,
		EffectiveFrom:  now,
		EffectiveTo:    nil,
		RuleJSON:       ruleJSON,
		CreatedAt:      now,
		CreatedBy:      createdBy,
		Notes:          notes,
	}

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// step 1: 封盘当前 active（可能 0 行：首次发布）
		if err := tx.
			Model(&repo.PricingVersion{}).
			Where("tenant_id = ? AND model_service_id = ? AND rule_class = ? AND effective_to IS NULL",
				tenantID, modelServiceID, ruleClass).
			Update("effective_to", now).Error; err != nil {
			return fmt.Errorf("seal current: %w", err)
		}
		// step 2: insert 新 active
		if err := tx.Create(newRow).Error; err != nil {
			return fmt.Errorf("insert new: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("pricing: rotate: %w", err)
	}
	return newRow, nil
}

// ListHistory 列某 (tenant, model_service, rule_class) 的全部版本，最新在前。
func (s *PricingStore) ListHistory(
	ctx context.Context, tenantID string, modelServiceID int64, ruleClass string,
) ([]repo.PricingVersion, error) {
	if tenantID == "" {
		tenantID = "default"
	}
	if modelServiceID == 0 {
		return nil, errors.New("pricing: model_service_id required")
	}
	if ruleClass == "" {
		ruleClass = "standard"
	}
	var out []repo.PricingVersion
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND model_service_id = ? AND rule_class = ?",
			tenantID, modelServiceID, ruleClass).
		Order("effective_from DESC").
		Find(&out).Error; err != nil {
		return nil, fmt.Errorf("pricing: list history: %w", err)
	}
	return out, nil
}

// GetActive 当前 active 版本（admin UI 显示"当前价"）。
func (s *PricingStore) GetActive(
	ctx context.Context, tenantID string, modelServiceID int64, ruleClass string,
) (*repo.PricingVersion, error) {
	if tenantID == "" {
		tenantID = "default"
	}
	if modelServiceID == 0 {
		return nil, errors.New("pricing: model_service_id required")
	}
	if ruleClass == "" {
		ruleClass = "standard"
	}
	var pv repo.PricingVersion
	now := time.Now().UTC()
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND model_service_id = ? AND rule_class = ? "+
			"AND effective_from <= ? AND (effective_to IS NULL OR effective_to > ?)",
			tenantID, modelServiceID, ruleClass, now, now).
		Order("effective_from DESC").
		First(&pv).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("pricing: no active version: tenant=%s model_service_id=%d class=%s",
				tenantID, modelServiceID, ruleClass)
		}
		return nil, fmt.Errorf("pricing: get active: %w", err)
	}
	return &pv, nil
}
