package domain

import "time"

// PricingSnapshot 价格快照的指纹（不含价格本体）。
//
// **append-only 语义**：指针 PricingVersionID 指向 pricing_versions 的某一行，
// 那行的 rule_json 永远不变（admin 改价是 INSERT 新行 + 旧行封盘 effective_to）。
// 所以即使过了几个月，billing engine 拿这个 ID 反查仍然能算出准确的旧价格。
//
// 计量事件只携带指纹（约 80 字节），下游 Enrich 阶段按 PricingVersionID
// JOIN pricing_versions 拿 rule_json 算钱。
// 详见 docs/architecture/05-metering-billing.md。
type PricingSnapshot struct {
	ModelServiceID       int64     // 用于 model 维度聚合统计
	PricingVersionID     int64     // → pricing_versions.id；billing engine 据此查 rule_json
	PricingEffectiveFrom time.Time // 该价格版本生效起点；调试用
	RuleClass            string    // standard | enterprise_xxx | promo_xxx；同租户内多曲线区分
}
