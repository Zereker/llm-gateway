package protocol

import (
	"log/slog"
	"sync"

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
// DefaultLookup — 包装全局 vendor + translator registry
// =============================================================================

// handlerCache 进程级 Handler 缓存——key = (vendor, srcProto, ep.Protocol)。
//
// **为什么需要**：M3 Envelope 给每个请求 new 一个 DefaultLookup{}；dispatch 内
// eligibility + invoke 两条路径各 lookup 一次。如果不在包级共享，combined Handler
// 每次 lookup 都重建，combined 内部的 quirks 编译缓存 跟着失效——deployer 配的
// quirks JSON 每个请求都重 compile 一次。
//
// Handler 本身是 stateless（vendor + translator + 内部 sync.Map cache），并发安全；
// 同 (vendor, srcProto, target) 三元组的请求共享同一个 Handler 实例。endpoint 通过
// PrepareCall 参数传入，不绑定到 Handler。
//
// **eviction**：vendor × srcProto × upstreamProto 组合数量上界很小（<100），不做
// eviction；条目随进程一起结束。
var handlerCache sync.Map // key = "vendor|src|target" → Handler

// DefaultLookup 走全局 vendor + translator registry 组合 Handler。M3 Envelope
// 在 rc.Handlers 为 nil 时填这个值。
//
// **stateless**：所有缓存都在包级 handlerCache。零值即可用，per-request 创建零成本。
type DefaultLookup struct{}

// pivotProtocol 缺对组合回退用的中间协议（docs/02 §6a）。
//
// 选 OpenAI：事实上的行业中间语言——现有跨协议 translator 全部以它为一端
// （anthropic↔openai / openai→gemini / responses→openai），任何新协议接入时
// 优先写它跟 OpenAI 的互转对，组合 fallback 的覆盖自动最大化。
const pivotProtocol = domain.ProtoOpenAI

func (DefaultLookup) Get(ep *domain.Endpoint, srcProto domain.Protocol) Handler {
	if ep == nil || ep.Protocol == domain.ProtoUnknown {
		return nil
	}
	key := ep.Vendor + "|" + srcProto.String() + "|" + ep.Protocol.String()
	if h, ok := handlerCache.Load(key); ok {
		return h.(Handler)
	}
	ad := LookupFactory(ep.Vendor)
	if ad == nil {
		return nil
	}
	// 直连 translator 优先（高保真）；miss 时经 pivot 组合兜底（可能有损，
	// 双跳丢 pivot 表达不了的字段）。热门组合应尽快补直连实现——直连注册后
	// FindVia 自动优先命中，组合退位，调用方无感。
	tr := translator.FindVia(srcProto, ep.Protocol, pivotProtocol)
	if tr == nil {
		return nil
	}
	if translator.IsComposed(tr) {
		// handlerCache 保证同 (vendor, src, tgt) 只 warn 一次
		slog.Warn("protocol: no direct translator, using lossy pivot composition",
			"src", srcProto.String(), "tgt", ep.Protocol.String(),
			"pivot", pivotProtocol.String(), "vendor", ep.Vendor)
	}
	h := Combine(ad, tr)
	actual, _ := handlerCache.LoadOrStore(key, h)
	return actual.(Handler)
}

// ResetHandlerCache 清空 DefaultLookup 的 Handler 缓存——**仅供测试**。
// 跑 ResetFactories / translator.Reset 之后必须配套调，避免旧 Handler 引用了已删的 Factory。
func ResetHandlerCache() {
	handlerCache = sync.Map{}
}
