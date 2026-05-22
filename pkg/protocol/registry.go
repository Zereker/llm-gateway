package protocol

import (
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/translator"
)

// Lookup 请求级 Handler 查询端口——按 (endpoint, sourceProtocol) 动态组合 Handler。
//
// **设计动机**：协议组合是 per-请求的事，不是 init() 时穷举的事。
//   - endpoint 携带 Protocol 字段（deployer 在 SQL INSERT 时配置）——表明这条 endpoint 上游说什么协议
//   - 客户端进来用 sourceProtocol（M3 Envelope 写入 rc.Envelope.SourceProtocol）
//   - DefaultLookup.Get(ep, src) 按 (ep.Vendor, src, ep.Protocol) 三元组临时组合：
//       * LookupFactory(ep.Vendor) → vendor HTTP 实现
//       * translator.Find(src, ep.Protocol) → 协议转换器；src == ep.Protocol 时
//         返回 identity translator（已在 pkg/translator/identity 包注册）
//   - 拿不到 → return nil；eligibility filter 据此剔除候选
//
// **覆盖场景**：多租户 / 灰度——middleware（如 M2 Auth）按 tenant 注入自定义
// Lookup 实现，可以走自己的 vendor 集合 / 自定义 translator chain。
type Lookup interface {
	// Get 按 endpoint 的 vendor + protocol 和客户端 srcProto 组合 Handler。
	// 找不到 adapter 或没有对应 translator 时返回 nil。
	Get(ep *domain.Endpoint, srcProto domain.Protocol) Handler
}

// =============================================================================
// DefaultLookup — 包装全局 adapter + translator registry
// =============================================================================

// DefaultLookup 走全局 vendor + translator registry 组合 Handler。M3 Envelope
// 在 rc.Handlers 为 nil 时填这个值。
type DefaultLookup struct{}

func (DefaultLookup) Get(ep *domain.Endpoint, srcProto domain.Protocol) Handler {
	if ep == nil || ep.Protocol == domain.ProtoUnknown {
		return nil
	}
	ad := LookupFactory(ep.Vendor)
	if ad == nil {
		return nil
	}
	tr := translator.Find(srcProto, ep.Protocol)
	if tr == nil {
		return nil
	}
	return Combine(ad, tr)
}
