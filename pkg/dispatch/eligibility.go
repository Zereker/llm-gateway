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
//  1. handlers.Get(ep, env.SourceProtocol) 返 nil → 没有可用 Handler（缺 adapter
//     或缺翻译器或 ep.Protocol 为 unknown）→ 剔除
//  2. Handler.Capabilities().SupportedModalities 不含 env.Modality → 剔除
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
		mods := h.Capabilities().SupportedModalities
		if len(mods) > 0 && !containsModality(mods, env.Modality) {
			continue
		}
		out = append(out, ep)
	}
	return out
}

func containsModality(set []domain.Modality, want domain.Modality) bool {
	for _, m := range set {
		if m == want {
			return true
		}
	}
	return false
}
