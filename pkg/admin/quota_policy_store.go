package admin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/zereker/llm-gateway/pkg/repo"
)

// QuotaPolicyStore 用 gorm 写 quota_policies 表。
//
// rule_json shape（M6 RateLimit 解释；admin 不解析）：
//
//	{
//	  "default":   {"rpm":60, "tpm":100000, "rps":null, "concurrent_requests":null},
//	  "per_model": {"gpt-4o":{"rpm":10}, "gpt-4o-mini":{"rpm":100}}
//	}
//
// **可改**：跟 pricing_versions 不同，quota policy 可就地 UPDATE rule_json
// （限流不影响计费历史，调整即时生效）。
type QuotaPolicyStore struct {
	db *gorm.DB
}

func NewQuotaPolicyStore(db *gorm.DB) *QuotaPolicyStore {
	return &QuotaPolicyStore{db: db}
}

// GetByID admin UI 用（policy 引用都是 BIGINT id）。
func (s *QuotaPolicyStore) GetByID(ctx context.Context, id int64) (*repo.QuotaPolicy, error) {
	if id == 0 {
		return nil, errors.New("quota_policy: id required")
	}
	var p repo.QuotaPolicy
	if err := s.db.WithContext(ctx).
		Where("id = ? AND deleted_at IS NULL", id).
		First(&p).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("quota_policy: not found: id=%d", id)
		}
		return nil, fmt.Errorf("quota_policy: get by id: %w", err)
	}
	return &p, nil
}

// GetByName admin UI 用名字找 ID。
func (s *QuotaPolicyStore) GetByName(ctx context.Context, name string) (*repo.QuotaPolicy, error) {
	if name == "" {
		return nil, errors.New("quota_policy: name required")
	}
	var p repo.QuotaPolicy
	if err := s.db.WithContext(ctx).
		Where("name = ? AND deleted_at IS NULL", name).
		First(&p).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("quota_policy: not found: name=%s", name)
		}
		return nil, fmt.Errorf("quota_policy: get by name: %w", err)
	}
	return &p, nil
}

// List 列全部未删 policies。
func (s *QuotaPolicyStore) List(ctx context.Context) ([]repo.QuotaPolicy, error) {
	var out []repo.QuotaPolicy
	if err := s.db.WithContext(ctx).
		Where("deleted_at IS NULL").
		Order("id ASC").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("quota_policy: list: %w", err)
	}
	return out, nil
}

// Create 插入新 policy；name 必须 unique。
func (s *QuotaPolicyStore) Create(ctx context.Context, p *repo.QuotaPolicy) error {
	if p == nil || p.Name == "" || len(p.RuleJSON) == 0 {
		return errors.New("quota_policy: name and rule_json required")
	}
	p.Enabled = true
	now := time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = now
	}
	if err := s.db.WithContext(ctx).Create(p).Error; err != nil {
		return fmt.Errorf("quota_policy: create: %w", err)
	}
	return nil
}

// Update 改 description / rule_json / enabled。name 不可改（业务键）。
func (s *QuotaPolicyStore) Update(ctx context.Context, id int64, updates QuotaPolicyUpdates) error {
	if id == 0 {
		return errors.New("quota_policy: id required")
	}
	patch := map[string]any{}
	if updates.Description != nil {
		patch["description"] = *updates.Description
	}
	if updates.RuleJSON != nil {
		patch["rule_json"] = datatypes.JSON(*updates.RuleJSON)
	}
	if updates.Enabled != nil {
		patch["enabled"] = *updates.Enabled
	}
	if len(patch) == 0 {
		return errors.New("quota_policy: no updatable fields supplied")
	}
	res := s.db.WithContext(ctx).
		Model(&repo.QuotaPolicy{}).
		Where("id = ? AND deleted_at IS NULL", id).
		Updates(patch)
	if res.Error != nil {
		return fmt.Errorf("quota_policy: update: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("quota_policy: not found: id=%d", id)
	}
	return nil
}

// QuotaPolicyUpdates 是 Update 可改字段。
type QuotaPolicyUpdates struct {
	Description *string
	RuleJSON    *[]byte
	Enabled     *bool
}

// Delete 软删 set deleted_at = NOW()。
//
// **注意**：被 accounts/api_keys 引用的 policy 软删后引用方仍指向同一 id；
// 此时 M6 应按"该层不限"处理（视 policy.deleted_at 为不存在）。
func (s *QuotaPolicyStore) Delete(ctx context.Context, id int64) error {
	if id == 0 {
		return errors.New("quota_policy: id required")
	}
	now := time.Now().UTC()
	res := s.db.WithContext(ctx).
		Model(&repo.QuotaPolicy{}).
		Where("id = ? AND deleted_at IS NULL", id).
		Updates(map[string]any{"deleted_at": now})
	if res.Error != nil {
		return fmt.Errorf("quota_policy: delete: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("quota_policy: not found: id=%d", id)
	}
	return nil
}
