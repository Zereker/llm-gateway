package repo

import (
	"context"
)

// EndpointReader gateway 数据平面（M7 Schedule middleware）+ admin 读侧接口。
//
// gateway 用 PickForModel；admin 用 GetByID + List。
//
// v0.1：PickForModel 选第一个匹配 (model, group) 的 endpoint，无加权 / 无 Filter。
// v0.5+ 由 pkg/schedule 完整接管。
//
// 写权限归 admin（pkg/admin 用 gorm 实现 CRUD），不在本接口里。
//
// Implementations MUST be safe for concurrent use（多 gin handler goroutine 同时调用）。
type EndpointReader interface {
	// PickForModel 从 (model, group) 匹配的 endpoints 里选一个。
	// group 为空时按 "default" 处理；找不到时返回错误（M7 abort 503）。
	PickForModel(c context.Context, model, group string) (*Endpoint, error)

	// GetByID 按业务 ID 精确取一条；admin 编辑 / 详情用。
	GetByID(c context.Context, id string) (*Endpoint, error)

	// List 返回所有 endpoint。
	List(c context.Context) ([]*Endpoint, error)
}
