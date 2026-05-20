// Package eligibility 候选资格过滤：docs/architecture/03-endpoint-scheduling.md §3。
//
// **职责**：把"不具备承接当前请求能力"的 endpoint 在进入 Scheduler 之前剔除——
// 这类不算上游失败，不该进入 Scheduler.Report 触发 cooldown。
//
// **过滤规则**：
//
//	1. endpoint 必须支持 env.Modality
//	2. endpoint.NativeProtocol == env.SourceProtocol，或存在 (env.SourceProtocol,
//	   endpoint.NativeProtocol) translator
//	3. endpoint 缺 adapter / native protocol 非法 / translator 未注册 → 剔除
//
// **纯函数**：本包不带任何状态；输入候选 + envelope + registry reader，输出
// eligible candidates + 被剔除原因。
//
// 详见 docs/architecture/03-endpoint-scheduling.md §3。
package eligibility

import (
	"github.com/zereker/llm-gateway/pkg/domain"
)

// AdapterLookup 抽象 vendor → 是否注册 adapter（避免直接依赖 pkg/adapter）。
//
// 用 bool 返回；实现侧通常包一层 adapter.Get(vendor) != nil。
type AdapterLookup interface {
	Has(vendor string) bool
	// NativeProtocol 返回该 vendor adapter 的 native protocol；vendor 未注册返 ProtoUnknown。
	NativeProtocol(vendor string) domain.Protocol
	// SupportedModalities 返回该 vendor adapter 支持的 modality 列表；vendor 未注册返 nil。
	SupportedModalities(vendor string) []domain.Modality
}

// TranslatorLookup 抽象 (source, target) → 是否注册 translator。
type TranslatorLookup interface {
	Has(source, target domain.Protocol) bool
}

// Reason 单个 endpoint 被剔除的原因。
type Reason struct {
	EndpointID int64
	Code       string // adapter_missing | modality_unsupported | translator_missing | protocol_unknown
	Detail     string
}

// Result eligibility 过滤的结果。
type Result struct {
	Eligible []*domain.Endpoint
	Removed  []Reason
}

// Filter 按 docs/03 §3 三条规则过滤候选。
//
// 入参 endpoints 是 caller 已经按 (model, group) 拉好的全集；本函数只做
// 能力维度过滤，不读 DB。
func Filter(
	candidates []*domain.Endpoint,
	env *domain.RequestEnvelope,
	adapters AdapterLookup,
	translators TranslatorLookup,
) Result {
	if env == nil {
		return Result{Eligible: candidates}
	}
	var result Result
	for _, ep := range candidates {
		reason := check(ep, env, adapters, translators)
		if reason != nil {
			reason.EndpointID = ep.ID
			result.Removed = append(result.Removed, *reason)
			continue
		}
		result.Eligible = append(result.Eligible, ep)
	}
	return result
}

func check(ep *domain.Endpoint, env *domain.RequestEnvelope, adapters AdapterLookup, translators TranslatorLookup) *Reason {
	// 1. vendor 必须注册 adapter
	if !adapters.Has(ep.Vendor) {
		return &Reason{Code: "adapter_missing", Detail: "vendor " + ep.Vendor + " not registered"}
	}

	// 2. modality 必须支持
	mods := adapters.SupportedModalities(ep.Vendor)
	if len(mods) > 0 && !containsModality(mods, env.Modality) {
		return &Reason{Code: "modality_unsupported", Detail: "vendor " + ep.Vendor + " does not support " + env.Modality.String()}
	}

	// 3. native protocol 必须知道，且能从 sourceProto 翻译到 nativeProto
	native := endpointNativeProtocol(ep, adapters)
	if native == domain.ProtoUnknown {
		return &Reason{Code: "protocol_unknown", Detail: "endpoint native protocol not declared"}
	}
	if native == env.SourceProtocol {
		return nil // identity 短路：同协议永远有 translator
	}
	if !translators.Has(env.SourceProtocol, native) {
		return &Reason{Code: "translator_missing", Detail: "no translator for " + env.SourceProtocol.String() + " → " + native.String()}
	}
	return nil
}

// endpointNativeProtocol 取 endpoint 的 native protocol：
// 优先 ep.NativeProtocol 字段；缺失时退化为 vendor adapter 的 metadata。
func endpointNativeProtocol(ep *domain.Endpoint, adapters AdapterLookup) domain.Protocol {
	// 当前 domain.Endpoint 字段不直接带 NativeProtocol；通过 vendor adapter metadata 取
	return adapters.NativeProtocol(ep.Vendor)
}

func containsModality(set []domain.Modality, want domain.Modality) bool {
	for _, m := range set {
		if m == want {
			return true
		}
	}
	return false
}
