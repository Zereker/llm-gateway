package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// SQLPricingProvider 是 PricingProvider 的 sqlx 实现。
//
// **v0.1 直查 DB，无缓存**——每次 M5 都跑一次 SELECT。
// 索引 idx_active_lookup (tenant_id, model_service_id, rule_class, effective_from)
// 覆盖热路径；活跃行查询 ~1ms。
//
// 上量后（>1w QPS）再叠加本地缓存 + DB 通知失效；现在不预填这个坑。
type SQLPricingProvider struct {
	db *sqlx.DB
}

// NewSQLPricingProvider 用现成 *sqlx.DB 构造（不打开新连接）。
func NewSQLPricingProvider(db *sqlx.DB) *SQLPricingProvider {
	return &SQLPricingProvider{db: db}
}

const pvColumns = `id, tenant_id, model_service_id, rule_class,
	effective_from, effective_to, rule_json,
	created_at, created_by, notes`

// GetActive 实现 PricingProvider.GetActive。
//
// 活跃判定下推 SQL：避免 fetch 全部 history 后内存里筛。
// ORDER BY effective_from DESC LIMIT 1：保证选最近发布的（防 admin 误连开两条没封盘）。
func (p *SQLPricingProvider) GetActive(ctx context.Context, tenantID string, modelServiceID int64, ruleClass string, t time.Time) (*PricingVersion, error) {
	if tenantID == "" {
		return nil, errors.New("pricing: empty tenant_id")
	}
	if modelServiceID == 0 {
		return nil, errors.New("pricing: empty model_service_id")
	}
	if ruleClass == "" {
		ruleClass = "standard"
	}
	var pv PricingVersion
	err := p.db.GetContext(ctx, &pv, p.db.Rebind(
		`SELECT `+pvColumns+` FROM pricing_versions
		 WHERE tenant_id = ? AND model_service_id = ? AND rule_class = ?
		   AND effective_from <= ?
		   AND (effective_to IS NULL OR effective_to > ?)
		 ORDER BY effective_from DESC LIMIT 1`),
		tenantID, modelServiceID, ruleClass, t, t,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("pricing: no active version for tenant=%s model_service_id=%d class=%s at %s",
				tenantID, modelServiceID, ruleClass, t.UTC().Format(time.RFC3339))
		}
		return nil, fmt.Errorf("pricing: get active: %w", err)
	}
	return &pv, nil
}

// ListHistory 实现 PricingProvider.ListHistory。
func (p *SQLPricingProvider) ListHistory(ctx context.Context, tenantID string, modelServiceID int64, ruleClass string) ([]*PricingVersion, error) {
	if tenantID == "" {
		return nil, errors.New("pricing: empty tenant_id")
	}
	if modelServiceID == 0 {
		return nil, errors.New("pricing: empty model_service_id")
	}
	if ruleClass == "" {
		ruleClass = "standard"
	}
	var rows []PricingVersion
	if err := p.db.SelectContext(ctx, &rows, p.db.Rebind(
		`SELECT `+pvColumns+` FROM pricing_versions
		 WHERE tenant_id = ? AND model_service_id = ? AND rule_class = ?
		 ORDER BY effective_from DESC`),
		tenantID, modelServiceID, ruleClass,
	); err != nil {
		return nil, fmt.Errorf("pricing: list history: %w", err)
	}
	out := make([]*PricingVersion, len(rows))
	for i := range rows {
		out[i] = &rows[i]
	}
	return out, nil
}

// 编译期断言。
var _ PricingProvider = (*SQLPricingProvider)(nil)
