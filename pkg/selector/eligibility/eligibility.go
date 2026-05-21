// Package eligibility 候选资格过滤：docs/architecture/03-endpoint-scheduling.md §3。
//
// **职责**：把"不具备承接当前请求能力"的 endpoint 在进入 Scheduler 之前剔除——
// 这类不算上游失败，不该进入 Scheduler.Report 触发 cooldown。
//
// **过滤规则**（v0.6 融合后）：
//
//  1. endpoint 必须支持 env.Modality（按 protocol.Handler 的 Capabilities）
//  2. 必须能为 (endpoint, env.SourceProtocol) 组合出 Handler——即 vendor adapter
//     + (srcProto → ep.Protocol) translator 都存在；缺一即剔除
//
// **纯函数**：本包不带任何状态；输入候选 + envelope + handler lookup，输出
// eligible candidates + 被剔除原因。
//
// 详见 docs/architecture/03-endpoint-scheduling.md §3。
package eligibility

import (
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// Reason 单个 endpoint 被剔除的原因。
type Reason struct {
	EndpointID int64
	Code       string // handler_missing | modality_unsupported
	Detail     string
}

// Result eligibility 过滤的结果。
type Result struct {
	Eligible []*domain.Endpoint
	Removed  []Reason
}

// Filter 按 docs/03 §3 规则过滤候选。
//
// 入参 endpoints 是 caller 已经按 (model, group) 拉好的全集；本函数只做
// 能力维度过滤，不读 DB。
//
// **v0.6 融合**：原 v0.5 用两个独立 lookup (AdapterLookup + TranslatorLookup)
// 三步检查 (adapter 存在 / modality 匹配 / translator 存在)，现在合成单一
// handlers.Get(ep, srcProto) 调用：返回 nil 代表"没法接这条 endpoint"，
// 不论是缺 adapter 还是缺 translator。modality 检查走 Handler.Capabilities。
func Filter(candidates []*domain.Endpoint, env *domain.RequestEnvelope, handlers protocol.Lookup) Result {
	if env == nil {
		return Result{Eligible: candidates}
	}
	var result Result
	for _, ep := range candidates {
		reason := check(ep, env, handlers)
		if reason != nil {
			reason.EndpointID = ep.ID
			result.Removed = append(result.Removed, *reason)
			continue
		}
		result.Eligible = append(result.Eligible, ep)
	}
	return result
}

func check(ep *domain.Endpoint, env *domain.RequestEnvelope, handlers protocol.Lookup) *Reason {
	if handlers == nil {
		return &Reason{Code: "handler_missing", Detail: "no handler lookup configured"}
	}
	h := handlers.Get(ep, env.SourceProtocol)
	if h == nil {
		return &Reason{
			Code:   "handler_missing",
			Detail: "no handler for vendor=" + ep.Vendor + " srcProto=" + env.SourceProtocol.String(),
		}
	}
	mods := h.Capabilities().SupportedModalities
	if len(mods) > 0 && !containsModality(mods, env.Modality) {
		return &Reason{
			Code:   "modality_unsupported",
			Detail: "vendor " + ep.Vendor + " does not support " + env.Modality.String(),
		}
	}
	return nil
}

func containsModality(set []domain.Modality, want domain.Modality) bool {
	for _, m := range set {
		if m == want {
			return true
		}
	}
	return false
}
