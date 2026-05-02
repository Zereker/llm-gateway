package envelope

import "encoding/json"

// CanonicalRequest 网关内部统一的请求形态（OpenAI 兼容 schema）。
//
// 只覆盖跨厂商共性的字段（起步 26 个）；专有字段不进 Canonical，在 RawBytes 中保留。
// 详见 docs/architecture/02-protocol-translation.md 第 3.2 节。
type CanonicalRequest struct {
	// 路由必需
	Model  string `json:"model"`
	Stream bool   `json:"stream"`

	// 通用聊天字段
	Messages    []Message `json:"messages,omitempty"`
	System      string    `json:"system,omitempty"` // Anthropic 风格的 system；OpenAI 风格走 Messages[0]
	MaxTokens   *int64    `json:"max_tokens,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
	TopP        *float64  `json:"top_p,omitempty"`
	TopK        *int64    `json:"top_k,omitempty"`
	Stop        []string  `json:"stop,omitempty"`

	// 工具与结构化输出
	Tools          []Tool          `json:"tools,omitempty"`
	ToolChoice     *ToolChoice     `json:"tool_choice,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`

	// 元信息
	User     string            `json:"user,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`

	// 透传扩展
	Reasoning *Reasoning `json:"reasoning,omitempty"`
	Thinking  *Thinking  `json:"thinking,omitempty"`

	// 多模态
	Modalities []string      `json:"modalities,omitempty"`
	Audio      *AudioOptions `json:"audio,omitempty"`
}

// Message 消息体（OpenAI / Anthropic 通用）。
type Message struct {
	Role    string          `json:"role"`    // system / user / assistant / tool
	Content json.RawMessage `json:"content"` // string 或 []ContentPart
	Name    string          `json:"name,omitempty"`

	// 工具调用
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Tool 工具声明。
type Tool struct {
	Type     string        `json:"type"` // 通常 "function"
	Function *ToolFunction `json:"function,omitempty"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // JSON Schema
}

type ToolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function *ToolCallFunc  `json:"function,omitempty"`
}

type ToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolChoice 控制工具选择策略。
type ToolChoice struct {
	Mode string         `json:"mode,omitempty"` // "auto" / "none" / "required" / "function"
	Func *ToolFuncChoice `json:"function,omitempty"`
}

type ToolFuncChoice struct {
	Name string `json:"name"`
}

// ResponseFormat 结构化输出格式。
type ResponseFormat struct {
	Type       string          `json:"type"`                  // "json_object" / "json_schema"
	JSONSchema json.RawMessage `json:"json_schema,omitempty"` // 用 type == "json_schema" 时
}

// Reasoning OpenAI o 系列推理选项。
type Reasoning struct {
	Effort string `json:"effort,omitempty"` // "low" / "medium" / "high"
}

// Thinking Anthropic 推理选项。
type Thinking struct {
	Type   string `json:"type,omitempty"`   // "enabled"
	Budget *int64 `json:"budget,omitempty"` // budget tokens
}

// AudioOptions 多模态音频请求选项。
type AudioOptions struct {
	Voice  string `json:"voice,omitempty"`
	Format string `json:"format,omitempty"`
}
