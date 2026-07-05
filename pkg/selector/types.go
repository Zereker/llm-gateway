// Package selector 端点选择 primitives——filter chain + scorer + picker。
//
// **设计精神**（docs/architecture/03-endpoint-scheduling.md §4）：
//
//   - 本包是**纯 selection primitives**：对一批候选跑 filter / scorer / picker
//     选 1 个 endpoint，无 per-request 状态
//   - 不持有 repo；不知道 protocol / handler / fallback / attempts 概念
//   - 跨 model fallback、attempts / excluded / decisions 状态、retry / abort
//     决策全在 `pkg/dispatch.Dispatcher` 维护——selector 永远只看一批候选
//
// **调用关系**（v0.6 起 dispatch 拥有调度时序，selector 退到 primitives 层）：
//
//	pkg/dispatch.Dispatcher.step (调度时序所有者)
//	    │  candidates = CandidateSource.ListForModel(ctx, model, group)
//	    │  eligible   = filterEligible(candidates, env, handlers)  // dispatch 内部 helper
//	    │  ep         = Selector.Pick(ctx, eligible, query)  ──→  selector.Scheduler.Pick
//	    │  ... Invoker.Invoke / Quota.Reserve / RetryPolicy.Decide ...
//	    │  Selector.Report(ctx, ep, verdict)              ──→    selector.Scheduler.Report
//	    │
//	    └─ adapter: pkg/dispatch/adapters/SelectorAdapter
//	       把 selector.Scheduler 桥成 dispatch.Selector（Pick 接受 eligible + PickQuery）
//
// **不应该出现在本包**：repo 依赖、http.Request、protocol.Handler、fallback 切 model 等。
//
// 详见 docs/architecture/03-endpoint-scheduling.md §4 + docs/architecture/03a-schedule-overview.md §0-§2。
package selector

import (
	"context"
	"time"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// Candidate 单个 endpoint 候选 + 有效权重。
//
// EffectiveWeight 是 Runtime Scoring（docs/03 §8）调整后的权重——
// 未启用 scoring 时由 dispatcher 简单填 = Endpoint.Weight。
type Candidate struct {
	Endpoint        *domain.Endpoint
	EffectiveWeight float64
}

// Request 单次 Pick 调用的入参。
//
// 按 docs/03 §4：Request 只承载一批候选，**不**包含 LoadFallback /
// FallbackModels / attempts 状态。
type Request struct {
	Model      string             // 当前 model（未路由前 = 请求 model；路由 fallback 时 = fallback model）
	Group      string             // 路由分组（rc.Identity.Group）
	SessionKey string             // 会话亲和 key（客户端 X-Gateway-Session 头）；空 = 不粘会话
	Candidates []Candidate        // 资格过滤后的候选（含 EffectiveWeight）
	ExcludeIDs map[int64]struct{} // 本次请求里已经尝试过的 endpoint
	PrefixKey  []byte             // PrefixCacheFilter 用的一致性哈希 key（跟 SessionKey 互补：
	//                              PrefixKey=按内容 prefix 无状态一致性哈希；SessionKey=客户端
	//                              显式 session id 的有状态 Redis 亲和，跟 weighted+scoring 组合）
}

// ErrorClass 把上游 / 网络 / 协议错误归类成几个粗粒度桶。
//
// CooldownManager 据此决定该 endpoint 该不该冷却 + 冷却多久。
type ErrorClass int

const (
	ClassUnknown   ErrorClass = iota // 分类不出来
	ClassSuccess                     // 2xx
	ClassTransient                   // 5xx / 网络错 / timeout / DNS
	ClassCapacity                    // 上游 429 / overloaded
	ClassPermanent                   // 上游 401 / 403 / 配置错
	ClassInvalid                     // 客户端 4xx（除 401/403/429）；不该重试
)

func (c ErrorClass) String() string {
	switch c {
	case ClassSuccess:
		return "success"
	case ClassTransient:
		return "transient"
	case ClassCapacity:
		return "capacity"
	case ClassPermanent:
		return "permanent"
	case ClassInvalid:
		return "invalid"
	default:
		return "unknown"
	}
}

// IsRetryable 决定 dispatch.RetryPolicy 是否继续 Pick 下一个候选。
//
//	Transient / Capacity / Permanent / Unknown → 重试
//	Success / Invalid                          → 停止
func (c ErrorClass) IsRetryable() bool {
	switch c {
	case ClassSuccess, ClassInvalid:
		return false
	default:
		return true
	}
}

// Result 一次调用的结果，由 dispatcher（通过 SelectorAdapter）传给 Scheduler.Report。
type Result struct {
	Class    ErrorClass
	HTTPCode int           // 上游 status；0 = 没拿到 response（网络错 / timeout）
	Reason   string        // 人读友好的错误描述
	Latency  time.Duration // 本次调用耗时（含上游 + 流式）
}

// Scheduler dispatch 通过 SelectorAdapter 调用的入口。无状态（docs/03 §4）。
type Scheduler interface {
	// Pick 输入当前 model 的候选 + 排除集，输出一个 endpoint。
	//
	// 返回 nil 表示候选全部过滤掉了（dispatch.FallbackPolicy.OnExhausted 决定 abort 503 还是切下一个 model）。
	Pick(ctx context.Context, req *Request) (*domain.Endpoint, error)

	// Report 把本次 Send 结果反馈给 cooldown / metric / stats store。
	// 不决定下一步控制流——dispatch.RetryPolicy.Decide 看 result.Class 决定继续 / 停止。
	Report(ctx context.Context, ep *domain.Endpoint, result Result)
}
