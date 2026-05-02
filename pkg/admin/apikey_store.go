package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/zereker-labs/ai-gateway/pkg/repo"
)

// APIKeyStore 用 gorm 写 api_keys 表。
//
// Create 服务端生成 api_key 明文（OpenAI/Stripe 套路）：调用方 POST 时只传
// user_id / group / external_user / expires_at，服务端生成 sk-XXX 随机串，
// **明文只在 Create 响应里返回一次**，后续 GET 只返回 api_key_id。
type APIKeyStore struct {
	db *gorm.DB
}

func NewAPIKeyStore(db *gorm.DB) *APIKeyStore {
	return &APIKeyStore{db: db}
}

// GetByAPIKeyID 按 (tenant_id, api_key_id) 查；用于 admin 详情 / 编辑。
//
// 注意：Get 走 api_key_id（稳定审计 ID），不走 api_key 字符串本身——
// 后者是凭证，admin 不该用它做 lookup。
func (s *APIKeyStore) GetByAPIKeyID(ctx context.Context, tenantID, apiKeyID string) (*repo.APIKey, error) {
	if tenantID == "" || apiKeyID == "" {
		return nil, errors.New("apikey: tenant_id and api_key_id required")
	}
	var k repo.APIKey
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND api_key_id = ?", tenantID, apiKeyID).
		First(&k).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("apikey: not found: tenant=%s api_key_id=%s", tenantID, apiKeyID)
		}
		return nil, fmt.Errorf("apikey: get: %w", err)
	}
	return &k, nil
}

// List 列租户范围内的 keys；可按 user_id / enabled 过滤（空字符串 / nil 表示不过滤）。
//
// userIDFilter == "" → 不按 user_id 过滤
// enabledFilter == nil → 不按 enabled 过滤；非 nil → 按值过滤
func (s *APIKeyStore) List(ctx context.Context, tenantID, userIDFilter string, enabledFilter *bool) ([]repo.APIKey, error) {
	if tenantID == "" {
		return nil, errors.New("apikey: tenant_id required")
	}
	q := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID)
	if userIDFilter != "" {
		q = q.Where("user_id = ?", userIDFilter)
	}
	if enabledFilter != nil {
		q = q.Where("enabled = ?", *enabledFilter)
	}
	var out []repo.APIKey
	if err := q.Order("id ASC").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("apikey: list: %w", err)
	}
	return out, nil
}

// Create 生成新 API key。
//
//   - 服务端 crypto/rand 生成 32 字节 hex（"sk-" + 64 hex chars，共 67 字符）
//   - 同时生成 api_key_id：`ak_<user_id>_<6字节 hex>`（12 字符 hex，共 ~24 字符）
//   - 调用方传 user_id / group / external_user / expires_at（可选）
//   - 成功后回填 k.ID + k.Key + k.APIKeyID + k.CreatedAt；**调用方负责**把 k.Key
//     在响应里返一次然后丢弃，不要存日志
func (s *APIKeyStore) Create(ctx context.Context, k *repo.APIKey) error {
	if k == nil || k.UserID == "" {
		return errors.New("apikey: user_id required")
	}
	if k.TenantID == "" {
		k.TenantID = "default"
	}
	if k.Group == "" {
		k.Group = "default"
	}
	if k.Key == "" {
		key, err := generateAPIKey()
		if err != nil {
			return fmt.Errorf("apikey: gen key: %w", err)
		}
		k.Key = key
	}
	if k.APIKeyID == "" {
		id, err := generateAPIKeyID(k.UserID)
		if err != nil {
			return fmt.Errorf("apikey: gen id: %w", err)
		}
		k.APIKeyID = id
	}
	k.Enabled = true // 默认启用；caller 可显式覆盖前
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now().UTC()
	}
	if err := s.db.WithContext(ctx).Create(k).Error; err != nil {
		return fmt.Errorf("apikey: create: %w", err)
	}
	return nil
}

// Update 按 (tenant_id, api_key_id) 更新可变字段：enabled / expires_at /
// group / external_user。**api_key 本身不可改**（要换 key 必须 Delete + Create）。
func (s *APIKeyStore) Update(ctx context.Context, tenantID, apiKeyID string, updates APIKeyUpdates) error {
	if tenantID == "" || apiKeyID == "" {
		return errors.New("apikey: tenant_id and api_key_id required")
	}
	patch := map[string]any{}
	if updates.Enabled != nil {
		patch["enabled"] = *updates.Enabled
	}
	if updates.ExpiresAt != nil {
		patch["expires_at"] = *updates.ExpiresAt
	}
	if updates.ClearExpiresAt {
		patch["expires_at"] = nil
	}
	if updates.Group != nil {
		patch["group_name"] = *updates.Group
	}
	if updates.ExternalUser != nil {
		patch["external_user"] = *updates.ExternalUser
	}
	if len(patch) == 0 {
		return errors.New("apikey: no updatable fields supplied")
	}
	res := s.db.WithContext(ctx).
		Model(&repo.APIKey{}).
		Where("tenant_id = ? AND api_key_id = ?", tenantID, apiKeyID).
		Updates(patch)
	if res.Error != nil {
		return fmt.Errorf("apikey: update: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("apikey: not found: tenant=%s api_key_id=%s", tenantID, apiKeyID)
	}
	return nil
}

// APIKeyUpdates 是 Update 可改字段；nil = 不动，非 nil = 改成新值。
// ClearExpiresAt = true 时把 expires_at 显式清成 NULL（"取消过期"）。
type APIKeyUpdates struct {
	Enabled        *bool
	ExpiresAt      *time.Time
	ClearExpiresAt bool
	Group          *string
	ExternalUser   *bool
}

func (s *APIKeyStore) Delete(ctx context.Context, tenantID, apiKeyID string) error {
	if tenantID == "" || apiKeyID == "" {
		return errors.New("apikey: tenant_id and api_key_id required")
	}
	res := s.db.WithContext(ctx).
		Where("tenant_id = ? AND api_key_id = ?", tenantID, apiKeyID).
		Delete(&repo.APIKey{})
	if res.Error != nil {
		return fmt.Errorf("apikey: delete: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("apikey: not found: tenant=%s api_key_id=%s", tenantID, apiKeyID)
	}
	return nil
}

// generateAPIKey 32 字节 crypto/rand → hex → "sk-" 前缀。
func generateAPIKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sk-" + hex.EncodeToString(b), nil
}

// generateAPIKeyID `ak_<user_id>_<12hex>`。userID 中如果有非字符串安全字符
// 只取前 24 字节（避免 schema VARCHAR(64) 溢出）。
func generateAPIKeyID(userID string) (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	if len(userID) > 24 {
		userID = userID[:24]
	}
	return fmt.Sprintf("ak_%s_%s", userID, hex.EncodeToString(b)), nil
}
