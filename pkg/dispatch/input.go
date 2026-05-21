package dispatch

import (
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// Input Dispatch 的只读输入——所有 dispatch driver loop 需要的请求级信息。
//
// **设计动机**：把 dispatch 从 *domain.RequestContext 解耦。RC 是 middleware
// 链的状态载体；dispatch 是协调器，只需要"这次请求要做什么"的纯数据视图。
//
// **构造点**：middleware/schedule.go 在调 Dispatch 之前从 rc 抽出来；不要在
// dispatch 内部创建。
//
// **字段语义**：
//
//	Envelope            ── M3 写：客户端 body + srcProto + modality
//	Identity            ── M2 写：account / api key / group
//	ModelChain          ── M5 写：[0]=primary，后续 = X-Gateway-Fallback-Models 校验过的
//	Handlers            ── M3 写：protocol.Lookup（DefaultLookup 或 tenant 覆盖）
//	AttemptCapOverride  ── 客户端 X-Gateway-Max-Attempts header 原始值；Policy 解析
type Input struct {
	Envelope           *domain.RequestEnvelope
	Identity           domain.UserIdentity
	ModelChain         []*domain.ModelService
	Handlers           protocol.Lookup
	AttemptCapOverride string
}

// PrimaryModel ModelChain 第一个；为空时返 nil（dispatcher 应早期校验 ModelChain 非空）。
func (in Input) PrimaryModel() *domain.ModelService {
	if len(in.ModelChain) == 0 {
		return nil
	}
	return in.ModelChain[0]
}

// SourceProtocol envelope.SourceProtocol；Envelope nil 时返 ProtoUnknown。
func (in Input) SourceProtocol() domain.Protocol {
	if in.Envelope == nil {
		return domain.ProtoUnknown
	}
	return in.Envelope.SourceProtocol
}
