package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// QuotaPolicyProvider M6 RateLimit middleware 用：按 ID 加载限流策略。
//
// 只声明读方法（admin 写走 pkg/admin.QuotaPolicyStore）。
//
// **v0.5 直查 DB，无缓存**——M6 每请求 2 次（ account + apikey policy）。
// 真上量后加 LRU + TTL；schema 不变。
//
// Implementations MUST be safe for concurrent use。
type QuotaPolicyProvider interface {
	// GetByID 取指定 policy；找不到 / 已软删 / disabled 都返回 (nil, nil)
	// 让 M6 当作"该层不限"——避免 admin 临时禁用 policy 时锁死所有引用方。
	GetByID(ctx context.Context, id int64) (*QuotaPolicy, error)
}

// SQLQuotaPolicyProvider sqlx 实现。
type SQLQuotaPolicyProvider struct {
	db *sqlx.DB
}

func NewSQLQuotaPolicyProvider(db *sqlx.DB) *SQLQuotaPolicyProvider {
	return &SQLQuotaPolicyProvider{db: db}
}

const qpColumns = `id, name, description, rule_json, enabled,
	created_at, updated_at, deleted_at`

// GetByID 实现 QuotaPolicyProvider.GetByID。
//
// 找不到 / 软删 / disabled 都返回 (nil, nil)。M6 看到 nil 当"该层不限"处理。
func (p *SQLQuotaPolicyProvider) GetByID(ctx context.Context, id int64) (*QuotaPolicy, error) {
	if id == 0 {
		return nil, nil
	}
	var pv QuotaPolicy
	err := p.db.GetContext(ctx, &pv, p.db.Rebind(
		`SELECT `+qpColumns+` FROM quota_policies
		 WHERE id = ? AND enabled = 1 AND deleted_at IS NULL
		 LIMIT 1`),
		id,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("quota_policy: get by id: %w", err)
	}
	return &pv, nil
}

// 编译期断言。
var _ QuotaPolicyProvider = (*SQLQuotaPolicyProvider)(nil)
