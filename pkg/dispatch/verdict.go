package dispatch

import "time"

// Verdict 一次 Invoker.Invoke 的结果分类。
//
// 字段沿用 selector.Result 的语义，但归属本包——Selector / Invoker / Policy
// 之间的共享词汇，不依赖 pkg/schedule。
type Verdict struct {
	Class    Class         // 粗粒度分类（决定 cooldown TTL + Decide → Action）
	HTTPCode int           // 上游 status；0 = 没拿到 response（网络错 / timeout）
	Reason   string        // 人读友好的错误描述
	Latency  time.Duration // 本次调用耗时（含上游 + 流式）
}

// Class 把上游 / 网络 / 协议错误归类成 6 个粗粒度桶。
//
// **语义不变**与 selector.ErrorClass 一致；改名是把"调度抽象"重命名为
// "判定抽象"——Class 是 driver 看到的"这次调用是个什么性质"，而不是
// "调度器内部的错误类型"。
type Class int

const (
	ClassUnknown   Class = iota // 分类不出来（IsRetryable = true，但 Selector.Report 不写 cooldown）
	ClassSuccess                // 2xx + 协议层成功
	ClassTransient               // 5xx / 网络错 / timeout / DNS
	ClassCapacity                // 上游 429 / overloaded / 本地 reserve 超限
	ClassPermanent               // 上游 401 / 403 / 配置错
	ClassInvalid                 // 客户端 4xx（除 401/403/429）/ translator 失败；不该重试
)

// String 用于 metric label / Attempt.ErrorClass 字段。
func (c Class) String() string {
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

// IsRetryable Class 是否值得换 endpoint 重试。
//
//	Transient / Capacity / Permanent / Unknown → 重试（换 ep 可能成功）
//	Success / Invalid                          → 不重试
//
// **注意**：Unknown 虽 retryable，但不写 cooldown（避免分类盲区污染冷却）。
// 那个特殊处理在 Selector 内部完成；Class.IsRetryable 仍按"是否换 ep"语义。
func (c Class) IsRetryable() bool {
	switch c {
	case ClassSuccess, ClassInvalid:
		return false
	default:
		return true
	}
}
