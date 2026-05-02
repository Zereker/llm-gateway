package repo

import (
	"context"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// ModelServiceReader M5 ModelService middleware 的依赖（只读）。
//
// gateway 数据平面只用 GetByModel；List 留给运维 / 启动诊断。
//
// Implementations MUST be safe for concurrent use（多 gin handler goroutine 同时调用）。
type ModelServiceReader interface {
	GetByModel(c context.Context, model string) (*domain.ModelServiceSnapshot, error)
	List(c context.Context) ([]*domain.ModelServiceSnapshot, error)
}

// ModelServiceWriter admin CRUD 用的写侧接口。
//
// Update / Delete 都按 model 字段（业务唯一标识）查找。
//
// Create 成功后会回填 snap.ID（auto-increment）。
type ModelServiceWriter interface {
	Create(c context.Context, snap *domain.ModelServiceSnapshot) error
	Update(c context.Context, snap *domain.ModelServiceSnapshot) error
	Delete(c context.Context, model string) error
}

// ModelServiceRepository = Reader + Writer，admin handler 一般依赖这个组合。
type ModelServiceRepository interface {
	ModelServiceReader
	ModelServiceWriter
}
