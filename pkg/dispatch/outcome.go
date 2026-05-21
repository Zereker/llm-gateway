package dispatch

import "github.com/zereker/llm-gateway/pkg/domain"

// Outcome Dispatch 的最终产出，由 middleware 翻译成 HTTP + 写回 RC。
//
// **语义**：
//
//	Result == OutcomeStreamed ── 响应已通过 Result.StreamTo 写到 w，middleware 不要再写
//	Result != OutcomeStreamed ── middleware 需根据 HTTPCode / Class / Reason 写错误响应
//
// **Decision**：永远填（即使 attempt = 0 也会写 SchedulingDecision，便于审计 / log）。
// **StreamErr**：仅 Result == OutcomeStreamed 时可能非 nil；流式过程中失败
// （header 已写、字节已发，无法回滚）。
// **RoutedModel**：实际成功的 model（fallback 时 != Input.PrimaryModel()）；
// Result != OutcomeStreamed 时为 nil。
// **Error**：dispatcher 不直接写 rc.Error，把 AdapterError 放在 Outcome 里让 middleware 写回 RC。
type Outcome struct {
	Result      OutcomeResult
	HTTPCode    int    // 仅 Result != OutcomeStreamed 时有意义
	Class       Class  // 仅 Result == OutcomeAbort 时有意义
	Reason      string // 仅 Result != OutcomeStreamed 时有意义
	Decision    *domain.SchedulingDecision
	Usage       *domain.Usage         // 仅 Result == OutcomeStreamed 时填
	StreamErr   error                 // 仅 Result == OutcomeStreamed + 流式中失败
	TTFTMs      int64                 // 仅 Result == OutcomeStreamed
	RoutedModel *domain.ModelService  // 实际成功 model；非 streamed 时为 nil
	Error       *domain.AdapterError  // stream 阶段错误的 typed 包装；nil = 无错
}

// OutcomeResult Dispatch 终态。
type OutcomeResult int

const (
	OutcomeUnknown   OutcomeResult = iota
	OutcomeStreamed                // 成功，response 已 stream 给客户端
	OutcomeInvalid                 // 客户端错（400）
	OutcomeTerminal                // 上游非 retryable 错（502）
	OutcomeNoEndpoint              // 所有 model / attempt 耗尽（503）
	OutcomeDepFail                 // Selector 依赖故障（503，Reason 含 SQL/Redis 错）
)

func (r OutcomeResult) String() string {
	switch r {
	case OutcomeStreamed:
		return "streamed"
	case OutcomeInvalid:
		return "invalid"
	case OutcomeTerminal:
		return "terminal"
	case OutcomeNoEndpoint:
		return "no_endpoint"
	case OutcomeDepFail:
		return "dep_fail"
	default:
		return "unknown"
	}
}
