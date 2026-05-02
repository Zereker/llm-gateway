// Package modelservice 定义模型路由配置与价格指纹。
//
// Snapshot 是 M5 ModelService middleware 的产物，由 Loader 接口实现。
package modelservice

import (
	"context"
	"encoding/json"
	"time"
)

// Snapshot 模型对外暴露的路由配置。
type Snapshot struct {
	ID         int64           // 内部唯一 ID（用于计量事件）
	ServiceID  string          // 业务唯一标识（如 "openai/gpt-4o"）
	Model      string          // 客户端可见的模型名（如 "gpt-4o"）
	UpdateTime time.Time       // 配置最后更新时间；与 ID 共同构成 PricingSnapshot 指纹
	SpecDetail json.RawMessage // 计量计价详细规格的 JSON；按需解析；详见 docs/architecture/05
	Group      string          // 默认 endpoint 组（供端点选择路由）
	Tpm        int64           // 默认每分钟 token 限额
	Rpm        int64           // 默认每分钟请求数限额
}

// PricingSnapshot 价格快照的指纹（不含价格本体）。
//
// 计量事件只携带指纹（约 50 字节），下游 Enrich 阶段按指纹查 history 表拿真实价格。
// 详见 docs/architecture/05-metering-billing.md。
type PricingSnapshot struct {
	ModelServiceID    int64
	ServiceUpdateTime time.Time
}

// Loader M5 ModelService middleware 的依赖接口。
//
// 默认实现走 ConfigStore + LRU 缓存（详见 docs/architecture/06）。
type Loader interface {
	GetByModel(ctx context.Context, model string) (*Snapshot, error)
	List(ctx context.Context) ([]*Snapshot, error)
}
