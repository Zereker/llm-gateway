// Package ctx 是网关的核心领域 god package：所有跨包共享的数据结构、最小接口在此定义。
//
// 设计原则：
//   - 零内部依赖（仅 stdlib + gin）；其他包通过 import "pkg/domain" 引用
//   - 不放业务逻辑，只放纯数据 / 最小接口
//   - 跨主题共享类型只下沉到本包，不到具体主题包
//
// 详见 docs/architecture/01-request-pipeline.md 与 _pkg目录结构.md。
package domain

// RequestContextKey 是 RequestContext 在 *gin.Context 上的唯一 key。
//
// 凡跨 middleware 状态都装进 *RequestContext，统一通过 pkg/middleware 包的
// GetRequestContext / TryGetRequestContext / AttachRequestContext 存取，
// 杜绝散落字符串 key。
const RequestContextKey = "ai_gateway.request_context"
