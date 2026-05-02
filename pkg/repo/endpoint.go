package repo

import (
	"context"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// EndpointReader M7 Schedule middleware + admin 读侧接口。
//
// gateway 用 PickForModel；admin 用 GetByID + List。
//
// v0.1：PickForModel 选第一个匹配 (model, group) 的 endpoint，无加权 / 无 Filter。
// v0.5+ 由 pkg/schedule 完整接管。
//
// Implementations MUST be safe for concurrent use（多 gin handler goroutine 同时调用）。
type EndpointReader interface {
	// PickForModel 从 (model, group) 匹配的 endpoints 里选一个。
	// group 为空时按 "default" 处理；找不到时返回错误（M7 abort 503）。
	PickForModel(c context.Context, model, group string) (*domain.Endpoint, error)

	// GetByID 按业务 ID 精确取一条；admin 编辑 / 详情用。
	GetByID(c context.Context, id string) (*domain.Endpoint, error)

	// List 返回所有 endpoint。
	List(c context.Context) ([]*domain.Endpoint, error)
}

// EndpointWriter admin CRUD 用的写侧接口。
//
// Update / Delete 都按 ID 查找。
type EndpointWriter interface {
	Create(c context.Context, ep *domain.Endpoint) error
	Update(c context.Context, ep *domain.Endpoint) error
	Delete(c context.Context, id string) error
}

// EndpointRepository = Reader + Writer。
type EndpointRepository interface {
	EndpointReader
	EndpointWriter
}
