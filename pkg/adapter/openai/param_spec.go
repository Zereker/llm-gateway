package openai

import "github.com/zereker-labs/ai-gateway/pkg/adapter"

// ParamSpec 实现 adapter.ParamSpecProvider，声明 OpenAI Chat Completions API 字段约束。
//
// **v0.5 限制**：仅声明数据；no enforcement middleware（见 pkg/adapter/param_spec.go 注释）。
//
// 字段范围参考 https://platform.openai.com/docs/api-reference/chat/create
//
// SupportedParams 是 OpenAI Chat Completions 的 chat 模态主流字段；reasoning models
// 的 `reasoning_effort` 等新参数也列了。Embeddings / images 等模态走自己的 ParamSpec
// （需要时按 modality 拆分；v0.5 用一套 catch-all）。
//
// Validators 只放高 ROI 的几个：temperature / top_p 越界时 clamp 而不是拒绝（多数厂商
// 这样做，避免无谓 4xx）。
func (Factory) ParamSpec() *adapter.ParamSpec {
	return &adapter.ParamSpec{
		SupportedParams: setOf(
			// chat 必填
			"model", "messages",
			// 控制
			"temperature", "top_p", "top_k", "max_tokens", "max_completion_tokens",
			"stop", "n", "seed", "logprobs", "top_logprobs",
			// 流式
			"stream", "stream_options",
			// 工具 / 结构化输出
			"tools", "tool_choice", "parallel_tool_calls", "response_format",
			// reasoning models
			"reasoning_effort",
			// 元信息
			"user", "metadata", "store",
			// 频率/惩罚
			"frequency_penalty", "presence_penalty", "logit_bias",
			// audio modality（chat completion 多模态）
			"audio", "modalities",
		),
		Validators: map[string]adapter.ParamValidator{
			"temperature": adapter.RangeValidator{Min: 0, Max: 2, OnOver: adapter.OverflowClamp},
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
