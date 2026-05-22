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
//  2. endpoint 不支持 env.Modality → 剔除：
//       - 先看 ep.Capabilities.Modalities（endpoint 级显式声明）：非空时**直接 authoritative**
//       - endpoint 没声明时 fall back 到 Handler.Capabilities().SupportedModalities
//         （vendor 级上限）
//     这样 vendor metadata 是"vendor 能支持的全集"，单条 endpoint 可以 narrow 到子集
//     （例：OpenAI vendor 声明 chat/embedding/image，但 deployer 给 ep-A 只买了 chat
//     quota，配 capabilities.modalities=["chat"] 锁死，不让该 ep 误接 /v1/embeddings）。
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

// endpointSupportsModality 按 endpoint 优先、vendor 兜底的顺序判模态资格。
func endpointSupportsModality(ep *domain.Endpoint, h protocol.Handler, want domain.Modality) bool {
	if len(ep.Capabilities.Modalities) > 0 {
		// endpoint 显式声明 → authoritative，vendor 不再 fallback
		return containsModality(ep.Capabilities.Modalities, want)
	}
	mods := h.Capabilities().SupportedModalities
	if len(mods) == 0 {
		// vendor 也没声明 → 视为不限模态（向后兼容历史 fakeAdapter 之类不填的实现）
		return true
	}
	return containsModality(mods, want)
}

func containsModality(set []domain.Modality, want domain.Modality) bool {
	for _, m := range set {
		if m == want {
			return true
		}
	}
	return false
}
