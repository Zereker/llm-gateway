package repo

import (
	"context"
)

// EndpointReader gateway 数据平面（M7 Schedule middleware）+ admin 读侧接口。
//
// **v0.3 改动**：去 accountID 参数——endpoints 是全局上游池，不再 per-account。
// 未来 BYOK（account 自带 endpoint）需要时再加 nullable account_id 过滤。
//
// gateway 用 PickForModel；admin 用 GetByID + List。
//
// PickForModel 选第一个匹配 (model, group) + enabled = 1 + deleted_at IS NULL 的 endpoint；
// 无加权 / 无 Filter。v0.5+ 由 pkg/schedule 完整接管。
//
// 写权限归 admin（pkg/admin 用 gorm 实现 CRUD），不在本接口里。
//
// Implementations MUST be safe for concurrent use（多 gin handler goroutine 同时调用）。
type EndpointReader interface {
	// ListForModel 返回 (model, group) 匹配的全部候选 endpoints，按 weight DESC 排序。
	// M7 LimitReadFilter 据此遍历做 endpoint quota 检查（第一个未超限的入选）。
	// 找不到任何候选时返回空切片 + nil error；M7 自己 abort 503。
	ListForModel(c context.Context, model, group string) ([]*Endpoint, error)

	// PickForModel 从 (model, group) 匹配的 endpoints 里选第一个；M7 v0.1 简化路径用。
	// 不参与 quota / cooldown / weight 等筛选——这些在 ListForModel + Filter 链里做。
	// 找不到时返回错误。
	PickForModel(c context.Context, model, group string) (*Endpoint, error)

	// GetByID 按 id 精确取一条；admin 编辑 / 详情用。
	GetByID(c context.Context, id int64) (*Endpoint, error)

	// List 返回所有未删 endpoint。
	List(c context.Context) ([]*Endpoint, error)
}
