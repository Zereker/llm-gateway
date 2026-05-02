package ctx

import (
	"encoding/json"
	"time"
)

// RequestEnvelope M3 Envelope middleware 的产物。
//
// 业务逻辑读 Parsed（结构化），透传到上游用 RawBytes（原始字节）。
// 这一双通道设计让"网关本身关心的字段（model / stream 等）"与
// "网关不关心但要保留的字段（reasoning_details / metadata 等）"完全解耦。
type RequestEnvelope struct {
	RawBytes       []byte
	Parsed         CanonicalRequest
	SourceProtocol Protocol
	Modality       Modality
	RequestTime    time.Time
}

// CanonicalRequest 网关内部统一的请求形态（OpenAI 兼容 schema）。
//
// 只覆盖跨厂商共性的字段（起步 26 个）；专有字段不进 Canonical，在 RawBytes 中保留。
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
	Role    string          `json:"role"` // system / user / assistant / tool
	Content json.RawMessage `json:"content"`
	Name    string          `json:"name,omitempty"`

	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Tool 工具声明。
type Tool struct {
	Type     string        `json:"type"`
	Function *ToolFunction `json:"function,omitempty"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type ToolCall struct {
	ID       string        `json:"id"`
	Type     string        `json:"type"`
	Function *ToolCallFunc `json:"function,omitempty"`
}

type ToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolChoice struct {
	Mode string          `json:"mode,omitempty"`
	Func *ToolFuncChoice `json:"function,omitempty"`
}

type ToolFuncChoice struct {
	Name string `json:"name"`
}

type ResponseFormat struct {
	Type       string          `json:"type"`
	JSONSchema json.RawMessage `json:"json_schema,omitempty"`
}

type Reasoning struct {
	Effort string `json:"effort,omitempty"`
}

type Thinking struct {
	Type   string `json:"type,omitempty"`
	Budget *int64 `json:"budget,omitempty"`
}

type AudioOptions struct {
	Voice  string `json:"voice,omitempty"`
	Format string `json:"format,omitempty"`
}

// CanonicalResponse Session.Finalize 的中间表示，仅在跨协议时使用。
type CanonicalResponse struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Created int64           `json:"created"`
	Choices []Choice        `json:"choices"`
	Usage   json.RawMessage `json:"usage,omitempty"`
	Raw     json.RawMessage `json:"-"`
}

type Choice struct {
	Index        int             `json:"index"`
	Message      *Message        `json:"message,omitempty"` // 非流式
	Delta        *Message        `json:"delta,omitempty"`   // 流式
	FinishReason string          `json:"finish_reason,omitempty"`
	Logprobs     json.RawMessage `json:"logprobs,omitempty"`
}
