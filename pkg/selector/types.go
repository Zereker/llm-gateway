// Package schedule M7 端点选路。
//
// **设计精神**（docs/architecture/03-endpoint-scheduling.md §4）：
//
//   - Scheduler 是**无状态**的批内选择器：Pick + Report 两个方法
//   - 不持有 repo / per-request 状态机；M7 维护 attempts / ExcludeIDs / decisions
//   - 跨 model fallback 由 M7 外层循环负责，scheduler 不知道 fallback
//
// **依赖**：
//
//	pkg/middleware/selector.go (M7)
//	    │
//	    ├─→ selector.Scheduler.Pick(ctx, req) → *Endpoint
//	    ├─→ Sender.Send(ctx, ep, env, body) → Outcome
//	    └─→ selector.Scheduler.Report(ctx, ep, result)
//
// 详见 docs/architecture/03-endpoint-scheduling.md。
package selector

import (
	"context"
	"time"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// Candidate 单个 endpoint 候选 + 有效权重。
//
// EffectiveWeight 是 Runtime Scoring（docs/03 §8）调整后的权重——
// 未启用 scoring 时由 M7 简单填 = Endpoint.Weight。
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
	Candidates []Candidate        // 资格过滤后的候选（含 EffectiveWeight）
	ExcludeIDs map[int64]struct{} // 本次请求里已经尝试过的 endpoint
	PrefixKey  []byte             // PrefixCacheFilter 用的一致性哈希 key
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

// IsRetryable 决定 M7 driver loop 是否继续 Pick 下一个候选。
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

// Result 一次调用的结果，由 M7 调用方传给 Scheduler.Report。
type Result struct {
	Class    ErrorClass
	HTTPCode int           // 上游 status；0 = 没拿到 response（网络错 / timeout）
	Reason   string        // 人读友好的错误描述
	Latency  time.Duration // 本次调用耗时（含上游 + 流式）
}

// Scheduler M7 调用入口。无状态（docs/03 §4）。
type Scheduler interface {
	// Pick 输入当前 model 的候选 + 排除集，输出一个 endpoint。
	//
	// 返回 nil 表示候选全部过滤掉了（M7 自己决定 abort 503 还是切下一个 model）。
	Pick(ctx context.Context, req *Request) (*domain.Endpoint, error)

	// Report 把本次 Send 结果反馈给 cooldown / metric / stats store。
	// 不决定下一步控制流——M7 自己看 result.Class 决定继续 / 停止。
	Report(ctx context.Context, ep *domain.Endpoint, result Result)
}
