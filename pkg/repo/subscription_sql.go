package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// SQLSubscriptionProvider 是 SubscriptionProvider 的 sqlx 实现。
//
// **v0.3 直查 DB，无缓存**——每次 M5 都跑一次 SELECT。
// 索引 uk_tenant_model (tenant_id, model_service_id) 直接命中，~1ms。
// 真上量后再加 caching。
type SQLSubscriptionProvider struct {
	db *sqlx.DB
}

func NewSQLSubscriptionProvider(db *sqlx.DB) *SQLSubscriptionProvider {
	return &SQLSubscriptionProvider{db: db}
}

// Has 实现 SubscriptionProvider.Has。
//
// SELECT 1 是最便宜的存在性检查；不取 row 字段。
// 条件：enabled = 1 AND deleted_at IS NULL（软禁用 / 软删都不算订阅）。
func (p *SQLSubscriptionProvider) Has(ctx context.Context, tenantID string, modelServiceID int64) (bool, error) {
	if tenantID == "" {
		return false, errors.New("subscription: empty tenant_id")
	}
	if modelServiceID == 0 {
		return false, errors.New("subscription: empty model_service_id")
	}
	var one int
	err := p.db.GetContext(ctx, &one, p.db.Rebind(
		`SELECT 1 FROM tenant_model_subscriptions
		 WHERE tenant_id = ? AND model_service_id = ?
		   AND enabled = 1 AND deleted_at IS NULL
		 LIMIT 1`),
		tenantID, modelServiceID,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("subscription: lookup: %w", err)
	}
	return true, nil
}

// 编译期断言。
var _ SubscriptionProvider = (*SQLSubscriptionProvider)(nil)
