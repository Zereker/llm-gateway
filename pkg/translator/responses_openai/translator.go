// Package responses_openai is the Translator for OpenAI Responses clients → OpenAI Chat Completions upstream.
//
// Clients send requests using the OpenAI Responses protocol (/v1/responses); this translator
// converts them to OpenAI Chat Completions format for the openai vendor upstream, and translates
// the response back into Responses format.
//
// **Protocol mapping**:
//
//	Request:
//	  {model, input, instructions, stream}
//	    → {model, messages: [{role:system, content:instructions}, {role:user, content:input}], stream}
//
//	Response (non-streaming):
//	  OpenAI Chat: {id, choices:[{message:{role,content}}], usage}
//	    → Responses: {id, object:"response", model, output:[{type:"message",role:"assistant",content:[{type:"output_text",text:"..."}]}], usage:{input_tokens,output_tokens,total_tokens}}
//
// **v1.0 limitations**:
//   - Does not support input arrays (message form); string input only
//   - Does not support tools / structured outputs
//   - Does not support previous_response_id (stateful continuation)
//   - Streaming: buffer-then-translate (v1.0 minimum)
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
// Request: Responses shape → OpenAI ChatCompletions shape
// =============================================================================

// responsesRequest is the Responses input shape.
type responsesRequest struct {
	Model        string          `json:"model"`
	Input        json.RawMessage `json:"input,omitempty"`        // string or []message
	Instructions string          `json:"instructions,omitempty"` // system prompt
	Stream       bool            `json:"stream,omitempty"`
	MaxTokens    *int            `json:"max_output_tokens,omitempty"`
	Temperature  *float64        `json:"temperature,omitempty"`
	TopP         *float64        `json:"top_p,omitempty"`
}

// chatMessage is a single OpenAI ChatCompletion message.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatRequest is the OpenAI ChatCompletion output shape (written by the translator).
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

	// input can be a string or []message; try string first
	var inputStr string
	if err := json.Unmarshal(req.Input, &inputStr); err == nil {
		if inputStr != "" {
			out.Messages = append(out.Messages, chatMessage{Role: "user", Content: inputStr})
		}
	} else {
		// try parsing as a message array (responses input messages shape)
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

// responsesInputMessage is the input message shape under the Responses protocol (partial support).
type responsesInputMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func (m responsesInputMessage) contentString() string {
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	// array shape: [{"type":"input_text","text":"..."}]
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
	return string(m.Content) // fallback
}

// =============================================================================
// Response: OpenAI ChatCompletions shape → Responses shape
// =============================================================================

// responseHandler is buffer-then-translate: accumulates the full response, then
// translates it all at once on Flush.
//
// In real streaming scenarios the OpenAI upstream sends SSE events; this is simplified
// for v1.0: accumulate all chunks (SSE or JSON) → on Flush, translate the whole thing
// into the final Responses format.
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

// openaiChatResponse is the OpenAI ChatCompletion response shape (full, non-streaming).
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

// responsesOutput is the Responses protocol output shape.
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

	// upstream may be SSE streaming; v1.0 minimum only supports a non-streaming JSON body
	// (translating fully-aggregated SSE is left for v1.1)
	var src openaiChatResponse
	if err := json.Unmarshal(h.buf, &src); err != nil {
		// upstream SSE can't be parsed as non-streaming → pass through the raw chunk as-is (the client may be able to handle it)
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
