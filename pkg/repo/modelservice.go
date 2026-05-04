package repo

import (
	"context"
)

// ModelServiceReader gateway 数据平面（M5 ModelService middleware）的依赖。
//
// **v0.3 改动**：去 tenantID 参数——model_services 是全局 catalog，不再 per-tenant；
// 模型可见性走 SubscriptionProvider。
//
// 只声明读方法——写权限归 admin（pkg/admin 用 gorm 实现 CRUD），不在本接口里。
//
// Implementations MUST be safe for concurrent use（多 gin handler goroutine 同时调用）。
type ModelServiceReader interface {
	GetByModel(c context.Context, model string) (*ModelService, error)
	List(c context.Context) ([]*ModelService, error)
}
