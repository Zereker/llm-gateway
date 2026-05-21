// Package responses_openai OpenAI Responses 客户端 → OpenAI Chat Completions 上游的 Translator。
//
// 客户端按 OpenAI Responses 协议发请求（/v1/responses）；本 translator 翻译成
// OpenAI Chat Completions 格式给 openai vendor 上游，响应再翻回 Responses 格式。
//
// **协议映射**：
//
//	Request:
//	  {model, input, instructions, stream}
//	    → {model, messages: [{role:system, content:instructions}, {role:user, content:input}], stream}
//
//	Response (non-streaming):
//	  OpenAI Chat: {id, choices:[{message:{role,content}}], usage}
//	    → Responses: {id, object:"response", model, output:[{type:"message",role:"assistant",content:[{type:"output_text",text:"..."}]}], usage:{input_tokens,output_tokens,total_tokens}}
//
// **v1.0 限制**：
//   - 不支持 input 数组（message 形态）；仅 string input
//   - 不支持 tools / structured outputs
//   - 不支持 previous_response_id（有状态延续）
//   - 流式：buffer-then-translate（v1.0 minimum）
package responses_openai

import (
	"encoding/json"
	"fmt"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/translator"
	"github.com/zereker/llm-gateway/pkg/usage/extractor"
)

type responsesOpenAI struct{}

func (responsesOpenAI) Source() domain.Protocol { return domain.ProtoResponses }
func (responsesOpenAI) Target() domain.Protocol { return domain.ProtoOpenAI }

func (responsesOpenAI) TranslateRequest(srcBody []byte) ([]byte, error) {
	return translateRequest(srcBody)
}

func (responsesOpenAI) NewResponseHandler() translator.ResponseHandler {
	return &responseHandler{ex: extractor.NewOpenAI()}
}

// =============================================================================
// Request：Responses 形态 → OpenAI ChatCompletions 形态
// =============================================================================

// responsesRequest Responses 入参 shape。
type responsesRequest struct {
	Model        string          `json:"model"`
	Input        json.RawMessage `json:"input,omitempty"`        // string 或 []message
	Instructions string          `json:"instructions,omitempty"` // system prompt
	Stream       bool            `json:"stream,omitempty"`
	MaxTokens    *int            `json:"max_output_tokens,omitempty"`
	Temperature  *float64        `json:"temperature,omitempty"`
	TopP         *float64        `json:"top_p,omitempty"`
}

// chatMessage OpenAI ChatCompletion 单 message。
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatRequest OpenAI ChatCompletion 出参 shape（translator 写）。
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Stream      bool          `json:"stream,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	TopP        *float64      `json:"top_p,omitempty"`
}

func translateRequest(srcBody []byte) ([]byte, error) {
	var req responsesRequest
	if err := json.Unmarshal(srcBody, &req); err != nil {
		return nil, fmt.Errorf("responses_openai: parse request: %w", err)
	}
	if req.Model == "" {
		return nil, fmt.Errorf("responses_openai: model required")
	}

	out := chatRequest{
		Model:       req.Model,
		Stream:      req.Stream,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}
	if req.Instructions != "" {
		out.Messages = append(out.Messages, chatMessage{Role: "system", Content: req.Instructions})
	}

	// input 可以是 string 或 []message；先尝试 string
	var inputStr string
	if err := json.Unmarshal(req.Input, &inputStr); err == nil {
		if inputStr != "" {
			out.Messages = append(out.Messages, chatMessage{Role: "user", Content: inputStr})
		}
	} else {
		// 尝试解析为 message 数组（responses input messages 形态）
		var inputMsgs []responsesInputMessage
		if err := json.Unmarshal(req.Input, &inputMsgs); err != nil {
			return nil, fmt.Errorf("responses_openai: input must be string or message array: %w", err)
		}
		for _, m := range inputMsgs {
			out.Messages = append(out.Messages, chatMessage{Role: m.Role, Content: m.contentString()})
		}
	}

	return json.Marshal(out)
}

// responsesInputMessage Responses 协议下的 input message 形态（部分支持）。
type responsesInputMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func (m responsesInputMessage) contentString() string {
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	// 数组形态：[{"type":"input_text","text":"..."}]
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var sb string
		for _, p := range parts {
			sb += p.Text
		}
		return sb
	}
	return string(m.Content) // 兜底
}

// =============================================================================
// Response：OpenAI ChatCompletions 形态 → Responses 形态
// =============================================================================

// responseHandler buffer-then-translate：累积完整响应后 Flush 时一次性翻译。
//
// 流式真实场景下 OpenAI 上游会发 SSE events；这里 v1.0 简化处理：累积所有 chunk
// （SSE 或 JSON）→ Flush 时整体翻译输出 Responses 终态格式。
type responseHandler struct {
	buf []byte
	ex  extractor.Session
}

func (h *responseHandler) Feed(chunk []byte) ([]byte, error) {
	if len(chunk) == 0 {
		return nil, nil
	}
	h.ex.Feed(chunk)
	h.buf = append(h.buf, chunk...)
	return nil, nil // buffer-then-translate
}

// openaiChatResponse OpenAI ChatCompletion 响应 shape（非流式整体）。
type openaiChatResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
}

// responsesOutput Responses 协议输出 shape。
type responsesOutput struct {
	ID     string `json:"id"`
	Object string `json:"object"` // "response"
	Model  string `json:"model"`
	Output []struct {
		Type    string `json:"type"` // "message"
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"` // "output_text"
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
		TotalTokens  int64 `json:"total_tokens"`
	} `json:"usage"`
}

func (h *responseHandler) Flush() ([]byte, *domain.Usage, error) {
	if len(h.buf) == 0 {
		return nil, h.ex.Final(), nil
	}

	// 上游可能是 SSE 流式；v1.0 minimum 只支持非流式 JSON body
	// （SSE 完整聚合后翻译留给 v1.1）
	var src openaiChatResponse
	if err := json.Unmarshal(h.buf, &src); err != nil {
		// 上游 SSE 无法非流式解析 → 直接透传原 chunk（客户端可能能消化）
		return h.buf, h.ex.Final(), nil
	}

	out := responsesOutput{
		ID:     "resp_" + stripPrefix(src.ID, "chatcmpl-"),
		Object: "response",
		Model:  src.Model,
	}
	if len(src.Choices) > 0 {
		c := src.Choices[0]
		out.Output = append(out.Output, struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}{
			Type: "message",
			Role: c.Message.Role,
			Content: []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}{{Type: "output_text", Text: c.Message.Content}},
		})
	}
	out.Usage.InputTokens = src.Usage.PromptTokens
	out.Usage.OutputTokens = src.Usage.CompletionTokens
	out.Usage.TotalTokens = src.Usage.TotalTokens

	body, err := json.Marshal(out)
	if err != nil {
		return nil, h.ex.Final(), err
	}
	return body, h.ex.Final(), nil
}

func stripPrefix(s, prefix string) string {
	if len(s) > len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):]
	}
	return s
}

func init() {
	translator.Register(responsesOpenAI{})
}
