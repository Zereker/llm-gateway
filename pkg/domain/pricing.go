package domain

import "time"

// PricingSnapshot 价格快照的指纹（不含价格本体）。
//
// 计量事件只携带指纹（约 50 字节），下游 Enrich 阶段按指纹查 history 表拿真实价格。
// 详见 docs/architecture/05-metering-billing.md。
type PricingSnapshot struct {
	ModelServiceID    int64
	ServiceUpdateTime time.Time
}
