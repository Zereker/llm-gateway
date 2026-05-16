package repo

import "context"

// SubscriptionProvider M5 ModelService middleware 用：判定主账号是否订阅了某 model_service。
//
// **fail-fast 语义**：M5 在拿到 model_service 后，必须查这张表确认订阅；找不到 → 403。
// 这是 SaaS 平台模型可见性的核心控制点（"哪个主账号看得到哪个模型"）。
//
// Implementations MUST be safe for concurrent use（多 gin handler goroutine 同时调用）。
type SubscriptionProvider interface {
	// Has 判定 (account_id, model_service_id) 是否订阅且 enabled 且未软删。
	// account_id 是历史列名，语义是主账号 pin。
	// 返回 (true, nil) = 已订阅；(false, nil) = 没订阅；(_, err) = SQL 出错。
	Has(ctx context.Context, accountID string, modelServiceID int64) (bool, error)
}
