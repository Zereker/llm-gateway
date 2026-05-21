package dispatch

import (
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// HandlersFrom 从 RequestContext 取 protocol.Lookup；nil / 类型不符时退化到
// protocol.DefaultLookup。
//
// **类型安全 helper**：rc.Handlers 声明为 any 是为了避 pkg/domain → pkg/dispatch
// → pkg/protocol → pkg/adapter → pkg/domain 循环依赖；所有消费者都走这个 helper，
// 不直接 type-assert。
//
// **设计动机**：v0.6 把 v0.5 split 的 AdapterLookup / TranslatorLookup 两个 port
// 融合成一个 protocol.Lookup —— consumer 只需要一个 lookup 拿到 (endpoint, srcProto)
// 对应的端到端 Handler，不再两次查找 + match 校验。多租户 / 灰度场景下 middleware
// （如 M2 Auth）可按 tenant 注入自定义 Lookup 实现。
func HandlersFrom(rc *domain.RequestContext) protocol.Lookup {
	if rc == nil {
		return protocol.DefaultLookup{}
	}
	if l, ok := rc.Handlers.(protocol.Lookup); ok && l != nil {
		return l
	}
	return protocol.DefaultLookup{}
}
