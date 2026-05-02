package middleware

import (
	"context"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// ModelServiceProvider M5 ModelService middleware 的依赖接口。
//
// 内置默认实现走 pkg/config.Store + 内存 LRU 缓存（首选），
// 也可以自定义实现接入数据库 / 远程 API。
//
// Implementations MUST be safe for concurrent use（多 gin handler goroutine 同时调用）。
type ModelServiceProvider interface {
	GetByModel(c context.Context, model string) (*domain.ModelServiceSnapshot, error)
	List(c context.Context) ([]*domain.ModelServiceSnapshot, error)
}
