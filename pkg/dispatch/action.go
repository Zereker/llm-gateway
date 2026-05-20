package dispatch

import "github.com/zereker/llm-gateway/pkg/domain"

// Action 是 Policy 决策的输出，driver loop 用 type switch 消费。
//
// 这是 sealed interface 模式——_action() 私有 marker 让外部包不能新增类型，
// 编译期保证 4 种 case 穷尽。
type Action interface {
	isAction()
}

// Continue 继续当前 model 的下一次 attempt（exclude 已尝试的 ep）。
type Continue struct{}

// Switch 切换到下一个 fallback model，重置 model-内的 attempt 状态。
type Switch struct {
	Next *domain.ModelService
}

// Stream verdict.Class == Success 时返回；driver 调 Result.StreamTo 流式响应。
type Stream struct{}

// Abort 终止本请求；driver 把 Class/HTTPCode/Reason 翻译成 HTTP 错误响应。
//
// Result 字段显式标识终态类型——HTTPCode 不足以区分 NoEndpoint(503 耗尽)
// vs DepFail(503 SQL/Redis 错)，Policy 实现要显式填。
type Abort struct {
	Result   OutcomeResult // Invalid / Terminal / NoEndpoint / DepFail
	Class    Class
	HTTPCode int
	Reason   string
}

func (Continue) isAction() {}
func (Switch) isAction()   {}
func (Stream) isAction()   {}
func (Abort) isAction()    {}
