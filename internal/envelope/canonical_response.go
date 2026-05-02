package envelope

import "encoding/json"

// CanonicalResponse Session.Finalize 的中间表示，仅在跨协议时使用。
//
// 同协议时 Session 通常直接把上游 chunk 原样写出，不构造 CanonicalResponse。
type CanonicalResponse struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Created int64           `json:"created"`
	Choices []Choice        `json:"choices"`
	Usage   json.RawMessage `json:"usage,omitempty"`

	// 透传上游原始 body 用于 debug
	Raw json.RawMessage `json:"-"`
}

// Choice 响应候选项。
type Choice struct {
	Index        int             `json:"index"`
	Message      *Message        `json:"message,omitempty"` // 非流式
	Delta        *Message        `json:"delta,omitempty"`   // 流式
	FinishReason string          `json:"finish_reason,omitempty"`
	Logprobs     json.RawMessage `json:"logprobs,omitempty"`
}
