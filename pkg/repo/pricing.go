package repo

import (
	"context"
	"time"
)

// PricingProvider M5 ModelService middleware 在请求路径上拍价格快照的依赖。
//
// **fail-fast 语义**：GetActive 找不到 active version → 返回错误，M5 据此
// abort 503。业务漏配价格立即可见，不会静悄悄欠账。
//
// 多租户 + 多规则：同一 model_service 在不同 tenant / rule_class 下可有完全
// 不同的价格曲线。v0.1 全局走 rule_class="standard"；将来 api_keys 加
// pricing_class 列时按身份选 class。
//
// Implementations MUST be safe for concurrent use（多 gin handler goroutine 同时调用）。
type PricingProvider interface {
	// GetActive 返回 (tenant, model_service, rule_class) 在 t 时刻活跃的价格版本。
	//
	// 活跃 = effective_from <= t AND (effective_to IS NULL OR effective_to > t)
	//
	// 找不到时返回带充分上下文的错误（M5 直接把 err.Error() 写进 abort message）。
	GetActive(ctx context.Context, tenantID string, modelServiceID int64, ruleClass string, t time.Time) (*PricingVersion, error)

	// ListHistory 列某 (tenant, model_service, rule_class) 的全部历史版本。
	// 按 effective_from 倒序，最新在前。admin UI 用。
	ListHistory(ctx context.Context, tenantID string, modelServiceID int64, ruleClass string) ([]*PricingVersion, error)
}
