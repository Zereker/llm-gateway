package repo

import (
	"context"
)

// ModelServiceReader gateway 数据平面（M5 ModelService middleware）的依赖。
//
// **v0.3 改动**：去 accountID 参数——model_services 是全局 catalog，不再 per-account；
// 模型可见性走 SubscriptionProvider。
//
// 只声明读方法——写直接走 SQL（model_services 表）。
//
// Implementations MUST be safe for concurrent use（多 gin handler goroutine 同时调用）。
type ModelServiceReader interface {
	GetByModel(c context.Context, model string) (*ModelService, error)
	List(c context.Context) ([]*ModelService, error)
}
