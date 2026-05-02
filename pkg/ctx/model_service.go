package ctx

import (
	"encoding/json"
	"time"
)

// ModelServiceSnapshot 模型对外暴露的路由配置（M5 ModelService middleware 产物）。
type ModelServiceSnapshot struct {
	ID         int64           // 内部唯一 ID（用于计量事件）
	ServiceID  string          // 业务唯一标识（如 "openai/gpt-4o"）
	Model      string          // 客户端可见的模型名（如 "gpt-4o"）
	UpdateTime time.Time       // 配置最后更新时间；与 ID 共同构成 PricingSnapshot 指纹
	SpecDetail json.RawMessage // 计量计价详细规格的 JSON；按需解析；详见 docs/architecture/05
	Group      string          // 默认 endpoint 组（供端点选择路由）
	Tpm        int64           // 默认每分钟 token 限额
	Rpm        int64           // 默认每分钟请求数限额
}
