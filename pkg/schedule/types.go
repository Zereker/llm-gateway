// Package schedule M7 端点选路完整版：Filter 链 + Cooldown + 跨 EP 重试 + WeightedRandom。
//
// **架构**：
//
//	pkg/middleware/schedule.go (M7)
//	    │
//	    └──→ schedule.Scheduler
//	             ├── BeginSelection() → Selection
//	             │       ├── Pick() 跑 Filter 链选下一个候选
//	             │       └── Report(ep, result) 标 cooldown / 记 attempt
//	             └── 内部组件
//	                  ├── Filter 链（cooldown / limit_read / weighted_random ...）
//	                  └── CooldownManager（Redis 共享）
//
// **重试模型**：M7 用 driver loop 编排——`for { ep := sel.Pick(); call(ep); sel.Report(...) }`。
// Selection 内部维护 epAttempts 计数 + pendingRetryEp 状态：
//   - **L1 同 ep 重试**：Transient（5xx / 网络抖动）且未到 max_per_endpoint → 下次 Pick 直接返回同 ep（不进 cooldown，避免误伤其它请求）
//   - **L2 跨 ep 重试**：L1 配额耗尽或非 Transient 失败 → cooldown + 跑 filter chain 选下一个
//   - max_attempts 是全局尝试上限；max_per_endpoint 是单 ep 上限（默认 1 = 无 L1）
//
// **不在 v0.5 范围**：
//   - HealthFilter（独立 health subsystem 自己一轮）
//   - PrefixCacheFilter / BusyFilter（self-hosted only，无 dev 测试场景）
//   - L3 跨 model fallback（pricing/usage 跨 model 语义复杂）
//
// 详见 docs/architecture/03-endpoint-scheduling.md。
package schedule

import (
	"context"
	"time"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// Request M7 选路所需的请求元信息。
type Request struct {
	Model               string // canonical model name（M5 ms.Model）
	Group               string // 路由分组（rc.Identity.Group）
	TPMCost             uint32 // M6 估算的 token cost；LimitReadFilter 用作 endpoint TPM bucket cost
	MaxAttemptsOverride int    // 0 = 用 cfg.scheduler.max_attempts；非 0 = 客户端 header 覆盖

	// FallbackModels L3 跨模型降级序列。当前 Model 的 candidates 全部跑完都失败时，
	// scheduler 按本数组顺序换 model 重 list candidates 再 try（attempts 计数继续累加，
	// 不重置；max_attempts 仍然是全局上限）。
	//
	// 留空 = L3 关闭（v0.5 默认行为）。
	//
	// 来源：M7 从 X-Gateway-Fallback-Models header（逗号分隔）读，或 admin 在
	// model_services 配 fallback 链。
	FallbackModels []string

	// PrefixKey 用于 PrefixCacheFilter 的一致性哈希 key；同 prefix 的请求路同 ep
	// 让 self-hosted 模型 KV-cache 命中。
	//
	// 来源：M7 从 rc.Envelope.RawBytes 取前 N bytes（避免大 body 影响哈希成本）。
	// 留空 = PrefixCacheFilter 退化成 noop（透传所有 candidates）。
	PrefixKey []byte
}

// ErrorClass 把上游 / 网络 / 协议错误归类成几个粗粒度桶；
// CooldownManager 据此决定该 endpoint 该不该冷却 + 冷却多久。
type ErrorClass int

const (
	ClassUnknown   ErrorClass = iota // 分类不出来
	ClassSuccess                      // 2xx
	ClassTransient                    // 5xx / 网络错 / timeout / DNS
	ClassCapacity                     // 上游 429 / overloaded
	ClassPermanent                    // 上游 401 / 403 / 配置错
	ClassInvalid                      // 客户端 4xx（除 401/403/429）；不该重试
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

// Result 一次调用的结果，由 M7 调用方传给 Selection.Report。
type Result struct {
	Class    ErrorClass
	HTTPCode int           // 上游 status；0 = 没拿到 response（网络错 / timeout）
	Reason   string        // 人读友好的错误描述
	Latency  time.Duration // 本次调用耗时（含上游 + 流式）
}

// Decision 一次尝试的完整记录；写到 rc.SchedulingDecision 给 M10 trace。
type Decision struct {
	AttemptNum int // 1-indexed
	EndpointID int64
	Vendor     string
	Result     Result
}

// CandidatesProvider 返回 (model, group) 匹配的全部候选；schedule 内部调用。
//
// 实现：repo.EndpointReader.ListForModel。
type CandidatesProvider interface {
	ListForModel(ctx context.Context, model, group string) ([]*domain.Endpoint, error)
}

// Scheduler M7 调用入口。
type Scheduler interface {
	// BeginSelection 拿候选 + 构造 per-request Selection 状态机。
	// 候选拿不到 / 完全空 → 返回 err（M7 abort 503）。
	BeginSelection(ctx context.Context, req *Request) (Selection, error)
}

// Selection 单次请求的选路状态机。
//
// **使用模式**（M7 driver loop）：
//
//	sel, err := scheduler.BeginSelection(ctx, req)
//	if err != nil { abort 503 }
//	defer sel.Done()
//
//	for {
//	    ep := sel.Pick()
//	    if ep == nil { break }
//
//	    result := callUpstream(ep)  // 拿 *http.Response 或 err，分类成 Result
//	    sel.Report(ep, result)
//
//	    if !result.Class.IsRetryable() {
//	        break  // success / invalid 立即结束
//	    }
//	    // transient / capacity / permanent / unknown → 试下一个
//	}
//
// **不并发**：Selection 在单 gin handler goroutine 内顺序使用。
type Selection interface {
	// Pick 下一个候选；nil = 用尽（max_attempts 到 / 候选耗尽）。
	Pick() *domain.Endpoint

	// Report 给本次 Pick 拿到的 ep 报告调用结果。
	// 失败 class 会触发 CooldownManager 标 cooldown。
	Report(ep *domain.Endpoint, result Result)

	// Decisions 返回截至当前的全部尝试链；M10 写 rc.SchedulingDecision。
	Decisions() []Decision

	// Done 释放资源（当前实现 no-op；保留扩展位）。
	Done()
}
