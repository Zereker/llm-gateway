package repo

import (
	"context"
)

// ModelServiceReader gateway 数据平面（M5 ModelService middleware）的依赖。
//
// 多租户：所有读方法都按 tenantID 范围内查；调用方从 rc.Identity.TenantID 取。
//
// 只声明读方法——写权限归 admin（pkg/admin 用 gorm 实现 CRUD），不在本接口里。
//
// Implementations MUST be safe for concurrent use（多 gin handler goroutine 同时调用）。
type ModelServiceReader interface {
	GetByModel(c context.Context, tenantID, model string) (*ModelService, error)
	List(c context.Context, tenantID string) ([]*ModelService, error)
}
