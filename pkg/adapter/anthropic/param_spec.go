package anthropic

import "github.com/zereker-labs/ai-gateway/pkg/adapter"

// ParamSpec 实现 adapter.ParamSpecProvider，声明 Anthropic Messages API 字段约束。
//
// 参考 https://docs.anthropic.com/en/api/messages
//
// **跟 OpenAI 的关键差异**（已被 openai_anthropic translator 在请求方向处理）：
//   - max_tokens 是 **必填**（OpenAI 可省）
//   - system 是顶层字段（OpenAI 走 messages[0]）
//   - stop_sequences 而不是 stop
//   - tools 的 schema 跟 OpenAI 不同（input_schema vs function.parameters）
//
// SupportedParams 列的是上游 Anthropic 真支持的字段（不是 canonical 名）。
func (Factory) ParamSpec() *adapter.ParamSpec {
	return &adapter.ParamSpec{
		SupportedParams: setOf(
			// 必填
			"model", "messages", "max_tokens",
			// 控制
			"temperature", "top_p", "top_k",
			"stop_sequences", "system", "stream",
			// 工具
			"tools", "tool_choice",
			// 元信息
			"metadata",
		),
		Validators: map[string]adapter.ParamValidator{
			"temperature": adapter.RangeValidator{Min: 0, Max: 1, OnOver: adapter.OverflowClamp},
			"top_p":       adapter.RangeValidator{Min: 0, Max: 1, OnOver: adapter.OverflowClamp},
		},
	}
}

func setOf(keys ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		m[k] = struct{}{}
	}
	return m
}

// 编译期断言 Factory 实现 adapter.ParamSpecProvider。
var _ adapter.ParamSpecProvider = Factory{}
