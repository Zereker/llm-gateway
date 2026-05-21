package selector

import (
	"context"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// EndpointReader 按 (model, group) 拉候选 endpoints 的 port。
//
// dispatch.Selector 的 SelectorAdapter（pkg/selector/dispatch.go）通过这个
// port 拿数据；repo SQL 实现通过 cmd/gateway/middleware_adapters.go 的
// adaptEndpoints 桥接到 repo.EndpointReader（领域接口更宽，把 ListForModel 拎出来）。
//
// **归属在 pkg/selector**：endpoint 候选拉取是 selector 的事，不是 middleware
// 的事；middleware 之前持有该接口是历史遗留（dispatch_wiring 在 cmd 时期跟 middleware
// 共用），v0.6 解耦后归位到 selector。
type EndpointReader interface {
	ListForModel(ctx context.Context, model, group string) ([]*domain.Endpoint, error)
}
