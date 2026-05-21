package middleware

import (
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// HandlersFrom 从 RequestContext 取 protocol.Lookup；nil / 类型不符时退化到
// protocol.DefaultLookup。
//
// **类型安全 helper**：rc.Handlers 声明为 any 是为了避 pkg/domain → pkg/protocol
// → pkg/adapter → pkg/domain 循环依赖；所有消费者都走这个 helper，不直接 type-assert。
//
// **归属在 middleware**：dispatch 已完全脱离 RequestContext；只有 middleware 层
// 才接触 RC，所以这个 RC ↔ typed lookup 桥接函数住在 middleware 里。
func HandlersFrom(rc *domain.RequestContext) protocol.Lookup {
	if rc == nil {
		return protocol.DefaultLookup{}
	}
	if l, ok := rc.Handlers.(protocol.Lookup); ok && l != nil {
		return l
	}
	return protocol.DefaultLookup{}
}
