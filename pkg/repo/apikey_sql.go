package repo

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// SQLAPIKeyProvider 是 IdentityProvider 的 MySQL 实现：
//
// **v0.3 改动**：JOIN tenants 一次拿全 (api_keys.quota_policy_id, tenants.quota_policy_id)，
// M6 RateLimit 不需要再额外 2 次 SELECT。
//
// **v0.2 改动**：DB 不再存明文 api_key——存 SHA-256 hash。Resolve 时把入参
// 先 hash 再查表。
//
// **v0.1 直查 DB，无缓存**——每次 Resolve 都跑一次 SELECT。
// api_key_hash 列上有 UNIQUE 索引，~1ms。
type SQLAPIKeyProvider struct {
	db *sqlx.DB
}

// NewSQLAPIKeyProvider 构造一个直查 DB 的 provider；不做启动期 load。
func NewSQLAPIKeyProvider(db *sqlx.DB) *SQLAPIKeyProvider {
	return &SQLAPIKeyProvider{db: db}
}

// resolveRow JOIN 后的行；只在本文件内部用。
type resolveRow struct {
	TenantID             string  `db:"tenant_id"`
	UserID               string  `db:"user_id"`
	APIKeyID             string  `db:"api_key_id"`
	Group                string  `db:"group_name"`
	ExternalUser         bool    `db:"external_user"`
	APIKeyQuotaPolicyID  *int64  `db:"apikey_quota_policy_id"`
	TenantQuotaPolicyID  *int64  `db:"tenant_quota_policy_id"`
}

// Resolve 实现 IdentityProvider.Resolve。
//
// SQL 一次 JOIN 拿两个 quota_policy_id；M6 RateLimit 直接消费 rc.Identity 不需要再查。
//
// 查询条件：
//   - api_key_hash = SHA256(creds.APIKey) hex-encoded
//   - api_keys.enabled = 1, revoked_at IS NULL, deleted_at IS NULL
//   - expires_at IS NULL OR expires_at > NOW()
//   - tenants.enabled = 1, tenants.deleted_at IS NULL（隐含的 pin 级别禁用）
//
// **不更新 last_used_at**：v0.3 不做（每请求 INSERT/UPDATE 等于 doubling 写压力）；
// 后续可改异步 batch update 走单独 goroutine。
func (p *SQLAPIKeyProvider) Resolve(ctx context.Context, creds *Credentials) (*UserIdentity, error) {
	if creds == nil || creds.APIKey == "" {
		return nil, errors.New("apikey: missing api key")
	}

	hashed := HashAPIKey(creds.APIKey)

	var row resolveRow
	err := p.db.GetContext(ctx, &row, p.db.Rebind(
		`SELECT
		    a.tenant_id           AS tenant_id,
		    a.user_id             AS user_id,
		    a.api_key_id          AS api_key_id,
		    a.group_name          AS group_name,
		    a.external_user       AS external_user,
		    a.quota_policy_id     AS apikey_quota_policy_id,
		    t.quota_policy_id     AS tenant_quota_policy_id
		 FROM api_keys a
		 JOIN tenants t ON t.pin = a.tenant_id
		 WHERE a.api_key_hash = ?
		   AND a.enabled = 1
		   AND a.revoked_at IS NULL
		   AND a.deleted_at IS NULL
		   AND (a.expires_at IS NULL OR a.expires_at > ?)
		   AND t.enabled = 1
		   AND t.deleted_at IS NULL
		 LIMIT 1`),
		hashed, time.Now().UTC(),
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("apikey: unknown / disabled / expired / revoked / tenant disabled")
		}
		return nil, fmt.Errorf("apikey: lookup: %w", err)
	}

	return &UserIdentity{
		TenantID:            row.TenantID,
		UserID:              row.UserID,
		APIKeyID:            row.APIKeyID,
		Group:               row.Group,
		ExternalUser:        row.ExternalUser,
		TenantQuotaPolicyID: row.TenantQuotaPolicyID,
		APIKeyQuotaPolicyID: row.APIKeyQuotaPolicyID,
	}, nil
}

// HashAPIKey SHA-256 hex-encode 入参；admin Create 和 gateway Resolve 共用。
func HashAPIKey(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// 编译期断言。
var _ IdentityProvider = (*SQLAPIKeyProvider)(nil)
