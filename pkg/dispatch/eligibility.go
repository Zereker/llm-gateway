package dispatch

import (
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// filterEligible 是 Dispatcher 内部的 eligibility 过滤步骤——剔除"不具备
// 承接当前请求能力"的 endpoint，让选 endpoint 这步不要被无效候选污染。
//
// **过滤规则**：
//
//  1. handlers.Get(ep, env.SourceProtocol) 返 nil → 没有可用 Handler（缺 vendor
//     Factory 或缺 translator 或 ep.Protocol 为 unknown）→ 剔除
//  2. endpoint 不支持 env.Modality → 剔除。语义是 **narrow 不能 widen**：
//       - endpoint 非空 + vendor 非空 → **两者都要包含**当前模态（intersection）
//       - endpoint 非空 + vendor 空   → 信 endpoint（兼容测试 stub 不填 metadata）
//       - endpoint 空   + vendor 非空 → 看 vendor 上限
//       - endpoint 空   + vendor 空   → 不限模态
//     deployer 错配 ["tts"] 在 chat-only vendor 上时也不会让请求偷溜进 selector。
//     典型用法：OpenAI vendor 声明 chat / embedding / image，deployer 给 ep-A
//     只买了 chat quota，配 capabilities.modalities=["chat"] 锁死。
//
// **归属在 dispatch**：v0.6 之前住在 pkg/selector/eligibility 是历史遗留；逻辑
// 跟 dispatch 流程（候选 → 过滤 → 选 → 调）强绑定，跟 selector 算法（filter
// chain / scorer / picker）无关，归到 dispatch 内部 helper 更合理。
//
// **纯函数**：不带状态；调用方拿到 eligible 列表后传给 Selector.Pick。
func filterEligible(candidates []*domain.Endpoint, env *domain.RequestEnvelope, handlers protocol.Lookup) []*domain.Endpoint {
	if env == nil {
		return candidates
	}
	if handlers == nil {
		return nil
	}
	out := make([]*domain.Endpoint, 0, len(candidates))
	for _, ep := range candidates {
		h := handlers.Get(ep, env.SourceProtocol)
		if h == nil {
			continue
		}
		if !endpointSupportsModality(ep, h, env.Modality) {
			continue
		}
		out = append(out, ep)
	}
	return out
}

// endpointSupportsModality 判 endpoint 是否能承接给定模态。
//
// **语义**：endpoint modalities 是 vendor 上限的 **narrowing 子集**，不能 widen。
//
//	endpoint 非空 + vendor 非空 → 两者都要支持（intersection）。deployer 配
//	  ["tts"] 但 vendor 只声明 chat 时，请求被剔除——防止 deployer 误配让
//	  本不支持的模态偷溜进 selector。
//	endpoint 非空 + vendor 空     → 信 endpoint（兼容 fakeAdapter / 测试 stub
//	  没填 vendor metadata 的实现）。
//	endpoint 空   + vendor 非空   → 看 vendor 上限。
//	endpoint 空   + vendor 空     → 不限模态。
func endpointSupportsModality(ep *domain.Endpoint, h protocol.Handler, want domain.Modality) bool {
	epMods := ep.Capabilities.Modalities
	vendorMods := h.Capabilities().SupportedModalities

	if len(epMods) == 0 && len(vendorMods) == 0 {
		return true
	}
	if len(epMods) > 0 && !containsModality(epMods, want) {
		return false
	}
	if len(vendorMods) > 0 && !containsModality(vendorMods, want) {
		return false
	}
	return true
}

func containsModality(set []domain.Modality, want domain.Modality) bool {
	for _, m := range set {
		if m == want {
			return true
		}
	}
	return false
}
