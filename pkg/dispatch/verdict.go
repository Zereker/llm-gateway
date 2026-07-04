package dispatch

import "time"

// Verdict 一次 Invoker.Invoke 的结果分类。
//
// 字段沿用 selector.Result 的语义，但归属本包——Selector / Invoker / Policy
// 之间的共享词汇，不依赖 pkg/schedule。
//
// **Stage 字段（v0.6 新加）**：标记本次失败发生在 dispatch 流水线哪一阶段，
// 让 Policy.Decide 可以做更细粒度的决策——例：StagePrepare 类失败（pre-call
// 协议转换失败）说明 endpoint 选错了 endpoint.Protocol；继续 retry 同 endpoint
// 没意义，可以直接 Switch 到下个 model 或 Abort。
type Verdict struct {
	Stage    Stage         // 本次失败发生在哪一阶段（v0.6 新加）；成功时 StageInvoke
	Class    Class         // 粗粒度分类（决定 cooldown TTL + Decide → Action）
	HTTPCode int           // 上游 status；0 = 没拿到 response（网络错 / timeout）
	Reason   string        // 人读友好的错误描述
	Latency  time.Duration // 本次调用耗时（含上游 + 流式）
}

// Stage 标记 dispatcher 流水线的阶段——Policy 据此区分"是哪一步失败"。
type Stage int

const (
	// StageInvoke 上游 HTTP 调用阶段（默认值——成功 / 网络错 / 上游 4xx/5xx 都属此阶段）。
	StageInvoke Stage = iota
	// StageSelect 选 endpoint 阶段（selector 依赖故障 / 候选耗尽）。
	StageSelect
	// StagePrepare pre-call 协议转换阶段（translator 失败 / vendor HTTP 构造失败）。
	// Policy 看到此 Stage 时，应跳过同 (endpoint.Protocol) 的其它 endpoint
	// （同协议组合下大概率同样失败）。
	StagePrepare
	// StageReserve endpoint ratelimit 前扣阶段（quota exhausted）。
	StageReserve
	// StageStream 响应流阶段（HTTP 200 之后、body 转发中失败——上游 RST /
	// 半途断流）。HTTP 状态已写出，无法回滚，也不能 retry；这个 Stage 只用于
	// Selector.Report / stats：一个"200 后掐断"的坏 endpoint 必须被 cooldown /
	// 打分看到，否则它在统计上永远是 100% success。
	StageStream
)

func (s Stage) String() string {
	switch s {
	case StageSelect:
		return "select"
	case StagePrepare:
		return "prepare"
	case StageReserve:
		return "reserve"
	case StageInvoke:
		return "invoke"
	case StageStream:
		return "stream"
	default:
		return "unknown"
	}
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
