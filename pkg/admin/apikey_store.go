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
// **v0.2 改动**：DB 不存明文——服务端生成 sk-XXX 后立刻 SHA-256 hash，
// 只存 hash + 前缀（"sk-abc1de2f"，admin UI 显示用）。明文只在 Create 返回值
// 给调用方用一次（POST 响应）；后续 GET 拿不到。
//
// 新增 Revoke 方法：set revoked_at = NOW()，区别于 Update enabled = 0。
type APIKeyStore struct {
	db *gorm.DB
}

func NewAPIKeyStore(db *gorm.DB) *APIKeyStore {
	return &APIKeyStore{db: db}
}

// GetByAPIKeyID 按 (tenant_id, api_key_id) 查未删 key。
//
// 注意：Get 走 api_key_id（稳定审计 ID），不走明文 / hash——后者是凭证，
// admin 不该用它做 lookup。
func (s *APIKeyStore) GetByAPIKeyID(ctx context.Context, tenantID, apiKeyID string) (*repo.APIKey, error) {
	if tenantID == "" || apiKeyID == "" {
		return nil, errors.New("apikey: tenant_id and api_key_id required")
	}
	var k repo.APIKey
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND api_key_id = ? AND deleted_at IS NULL", tenantID, apiKeyID).
		First(&k).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("apikey: not found: tenant=%s api_key_id=%s", tenantID, apiKeyID)
		}
		return nil, fmt.Errorf("apikey: get: %w", err)
	}
	return &k, nil
}

// List 列租户范围内未删 keys；可按 user_id / enabled 过滤（空 / nil 表示不过滤）。
func (s *APIKeyStore) List(ctx context.Context, tenantID, userIDFilter string, enabledFilter *bool) ([]repo.APIKey, error) {
	if tenantID == "" {
		return nil, errors.New("apikey: tenant_id required")
	}
	q := s.db.WithContext(ctx).Where("tenant_id = ? AND deleted_at IS NULL", tenantID)
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

// Create 生成新 API key，返回 (plaintext, error)。
//
//   - 服务端 crypto/rand 生成 32 字节 hex（"sk-" + 64 hex chars，共 67 字符）
//   - SHA-256(plaintext) → 64 hex chars 落 api_key_hash 列
//   - 前 12 字符（"sk-" + 9 hex）落 api_key_prefix 列，admin UI 显示
//   - api_key_id：`ak_<user_id>_<6字节 hex>`
//   - 调用方拿到 plaintext **只能返响应给客户一次**，绝不要存日志 / DB
func (s *APIKeyStore) Create(ctx context.Context, k *repo.APIKey) (plaintext string, err error) {
	if k == nil || k.UserID == "" {
		return "", errors.New("apikey: user_id required")
	}
	if k.TenantID == "" {
		k.TenantID = "default"
	}
	if k.Group == "" {
		k.Group = "default"
	}
	plaintext, err = generateAPIKey()
	if err != nil {
		return "", fmt.Errorf("apikey: gen key: %w", err)
	}
	k.APIKeyHash = repo.HashAPIKey(plaintext)
	k.APIKeyPrefix = plaintext[:12] // "sk-" + 9 hex chars
	if k.APIKeyID == "" {
		id, err := generateAPIKeyID(k.UserID)
		if err != nil {
			return "", fmt.Errorf("apikey: gen id: %w", err)
		}
		k.APIKeyID = id
	}
	k.Enabled = true
	now := time.Now().UTC()
	if k.CreatedAt.IsZero() {
		k.CreatedAt = now
	}
	if k.UpdatedAt.IsZero() {
		k.UpdatedAt = now
	}
	if err := s.db.WithContext(ctx).Create(k).Error; err != nil {
		return "", fmt.Errorf("apikey: create: %w", err)
	}
	return plaintext, nil
}

// Update 按 (tenant_id, api_key_id) 更新可变字段：enabled / expires_at /
// group / external_user / name。
//
// **api_key_hash / api_key_prefix 不可改**——要换 key 必须 Delete + Create
// （这正是设计意图：凭证泄露后只能 rotate 不能 patch）。
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
	if updates.Name != nil {
		patch["name"] = *updates.Name
	}
	if updates.QuotaPolicyID != nil {
		patch["quota_policy_id"] = *updates.QuotaPolicyID
	}
	if updates.ClearQuotaPolicy {
		patch["quota_policy_id"] = nil
	}
	if len(patch) == 0 {
		return errors.New("apikey: no updatable fields supplied")
	}
	res := s.db.WithContext(ctx).
		Model(&repo.APIKey{}).
		Where("tenant_id = ? AND api_key_id = ? AND deleted_at IS NULL", tenantID, apiKeyID).
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
// ClearQuotaPolicy = true 时把 quota_policy_id 清成 NULL（"取消 key 维度限流"）。
type APIKeyUpdates struct {
	Enabled          *bool
	ExpiresAt        *time.Time
	ClearExpiresAt   bool
	Group            *string
	ExternalUser     *bool
	Name             *string
	QuotaPolicyID    *int64
	ClearQuotaPolicy bool
}

// Revoke set revoked_at = NOW()。区别于 Delete（软删）/ Update enabled=false（暂停）。
//
// 语义：明确告诉审计 / 用户"这个 key 被主动吊销了"，跟自然过期 / 暂停区分。
// gateway Resolve 时 revoked_at IS NOT NULL → 拒绝。
func (s *APIKeyStore) Revoke(ctx context.Context, tenantID, apiKeyID string) error {
	if tenantID == "" || apiKeyID == "" {
		return errors.New("apikey: tenant_id and api_key_id required")
	}
	now := time.Now().UTC()
	res := s.db.WithContext(ctx).
		Model(&repo.APIKey{}).
		Where("tenant_id = ? AND api_key_id = ? AND deleted_at IS NULL", tenantID, apiKeyID).
		Updates(map[string]any{
			"revoked_at": now,
			"enabled":    false,
		})
	if res.Error != nil {
		return fmt.Errorf("apikey: revoke: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("apikey: not found: tenant=%s api_key_id=%s", tenantID, apiKeyID)
	}
	return nil
}

// Delete 软删——set deleted_at = NOW()。可恢复需要直接改 DB。
func (s *APIKeyStore) Delete(ctx context.Context, tenantID, apiKeyID string) error {
	if tenantID == "" || apiKeyID == "" {
		return errors.New("apikey: tenant_id and api_key_id required")
	}
	now := time.Now().UTC()
	res := s.db.WithContext(ctx).
		Model(&repo.APIKey{}).
		Where("tenant_id = ? AND api_key_id = ? AND deleted_at IS NULL", tenantID, apiKeyID).
		Updates(map[string]any{"deleted_at": now})
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

// generateAPIKeyID `ak_<user_id>_<12hex>`。userID 太长截断 24 字节避免 VARCHAR(64) 溢出。
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
