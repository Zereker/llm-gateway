package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// SQLAPIKeyProvider 是 IdentityProvider 的 MySQL 实现：
//
// **v0.1 直查 DB，无缓存**——每次 Resolve 都跑一次 SELECT。
// api_key 列上有 UNIQUE 索引，等于主键级查询（~1ms）；admin 改 key 立即生效。
//
// 真上量后（>1w QPS / 单实例 DB 顶不住）再叠加本地缓存 + redis pub/sub
// 失效通道；现在不预填这个坑。
type SQLAPIKeyProvider struct {
	db *sqlx.DB
}

// NewSQLAPIKeyProvider 构造一个直查 DB 的 provider；不做启动期 load。
func NewSQLAPIKeyProvider(db *sqlx.DB) *SQLAPIKeyProvider {
	return &SQLAPIKeyProvider{db: db}
}

// Resolve 实现 IdentityProvider.Resolve。
//
// 查询条件：api_key 命中 + enabled = 1 + (expires_at IS NULL OR expires_at > NOW())。
// 把过期判定下推 SQL，避免内存里反复算时间。
func (p *SQLAPIKeyProvider) Resolve(ctx context.Context, creds *Credentials) (*UserIdentity, error) {
	if creds == nil || creds.APIKey == "" {
		return nil, errors.New("apikey: missing api key")
	}

	var k APIKey
	err := p.db.GetContext(ctx, &k, p.db.Rebind(
		`SELECT id, tenant_id, api_key, api_key_id, user_id, group_name,
		        external_user, enabled, expires_at, created_at
		 FROM api_keys
		 WHERE api_key = ? AND enabled = 1
		   AND (expires_at IS NULL OR expires_at > ?)
		 LIMIT 1`),
		creds.APIKey, time.Now().UTC(),
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("apikey: unknown / disabled / expired api key")
		}
		return nil, fmt.Errorf("apikey: lookup: %w", err)
	}

	id := k.ToUserIdentity()
	return &id, nil
}

// 编译期断言。
var _ IdentityProvider = (*SQLAPIKeyProvider)(nil)
