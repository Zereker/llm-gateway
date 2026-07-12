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
// **Limitations**:
//   - tools and non-text input parts (input_image / input_file / ...) cannot
//     survive the translation, so they are rejected with an invalid error
//     (fail-fast) instead of being silently dropped — a dropped image would
//     still bill the request while the model answers "I see no image".
//     Full support requires a native Responses upstream (endpoint
//     protocol=responses), which routes through identity instead.
//   - Does not support previous_response_id (stateful continuation)
//   - Streaming: the upstream stream is buffered and returned as a single
//     Responses body (with include_usage-based billing); true incremental
//     Responses events are not emitted.
package responses_openai

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/translator"
	"github.com/zereker/llm-gateway/internal/usage/extractor"
)

type responsesOpenAI struct{}

// New returns the Responses-to-OpenAI translator.
func New() translator.Translator { return responsesOpenAI{} }

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
	Tools        json.RawMessage `json:"tools,omitempty"` // parsed only to fail-fast; never forwarded
}

// chatMessage is a single OpenAI ChatCompletion message.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatRequest is the OpenAI ChatCompletion output shape (written by the translator).
type chatRequest struct {
	Model         string          `json:"model"`
	Messages      []chatMessage   `json:"messages"`
	Stream        bool            `json:"stream,omitempty"`
	StreamOptions *chatStreamOpts `json:"stream_options,omitempty"`
	MaxTokens     *int            `json:"max_tokens,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
}

type chatStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

func translateRequest(srcBody []byte) ([]byte, error) {
	var req responsesRequest
	if err := json.Unmarshal(srcBody, &req); err != nil {
		return nil, fmt.Errorf("responses_openai: parse request: %w", err)
	}

	if req.Model == "" {
		return nil, fmt.Errorf("responses_openai: model required")
	}

	// Fail-fast: tool definitions cannot survive the translation to a chat
	// upstream; dropping them silently would break the caller's tool loop.
	if len(req.Tools) > 0 {
		var tools []json.RawMessage
		if err := json.Unmarshal(req.Tools, &tools); err != nil || len(tools) > 0 {
			return nil, fmt.Errorf("responses_openai: tools are not supported when routed to a chat upstream; use a native responses endpoint")
		}
	}

	out := chatRequest{
		Model:       req.Model,
		Stream:      req.Stream,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}
	if req.Stream {
		// The upstream only emits a final usage chunk when asked; without this
		// the extractor's side channel sees no usage and the request bills zero.
		out.StreamOptions = &chatStreamOpts{IncludeUsage: true}
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
			content, err := m.contentText()
			if err != nil {
				return nil, err
			}

			out.Messages = append(out.Messages, chatMessage{Role: m.Role, Content: content})
		}
	}

	return json.Marshal(out)
}

// responsesInputMessage is the input message shape under the Responses protocol (partial support).
type responsesInputMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func (m responsesInputMessage) contentText() (string, error) {
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s, nil
	}

	// array shape: [{"type":"input_text","text":"..."}]
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			// Fail-fast: a non-text part (input_image / input_file / ...) has
			// no chat equivalent here; dropping it would silently change what
			// the model sees while still billing the request.
			if p.Type != "input_text" && p.Type != "" {
				return "", fmt.Errorf("responses_openai: content part %q is not supported when routed to a chat upstream; use a native responses endpoint", p.Type)
			}

			sb.WriteString(p.Text)
		}

		return sb.String(), nil
	}

	return string(m.Content), nil // fallback
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
	ID        string `json:"id"`
	Object    string `json:"object"` // "response"
	Status    string `json:"status"` // always "completed" (buffer-then-translate never returns partials)
	CreatedAt int64  `json:"created_at"`
	Model     string `json:"model"`
	Output    []struct {
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

	var src openaiChatResponse
	if err := json.Unmarshal(h.buf, &src); err != nil {
		// The upstream body is SSE (the client requested streaming). Rather
		// than hand the client raw OpenAI chat chunks — which a Responses
		// client can't parse — aggregate the SSE into a single valid Responses
		// object. Usage comes from the extractor side channel (which now sees
		// the include_usage final chunk). This is a buffered (non-streamed)
		// Responses reply; true incremental Responses events are not emitted.
		content, model, id, created := aggregateChatSSE(h.buf)
		//nolint:nilerr // deliberate: an unmarshal failure means "the body is SSE, not JSON" and is handled by aggregating, not surfaced
		return h.buildResponse(id, model, "assistant", content, created), h.ex.Final(), nil
	}

	role, content := "assistant", ""
	if len(src.Choices) > 0 {
		role = src.Choices[0].Message.Role
		content = src.Choices[0].Message.Content
	}

	return h.buildResponse(src.ID, src.Model, role, content, src.Created), h.ex.Final(), nil
}

// buildResponse marshals a Responses-format body. The Responses `usage` object
// is populated from the extractor's Final().
func (h *responseHandler) buildResponse(id, model, role, content string, created int64) []byte {
	u := h.ex.Final()

	if created == 0 {
		created = time.Now().Unix()
	}

	out := responsesOutput{
		ID:        "resp_" + stripPrefix(id, "chatcmpl-"),
		Object:    "response",
		Status:    "completed",
		CreatedAt: created,
		Model:     model,
	}

	out.Output = append(out.Output, struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}{
		Type: "message",
		Role: role,
		Content: []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{{Type: "output_text", Text: content}},
	})
	if u != nil {
		out.Usage.InputTokens = u.Input
		out.Usage.OutputTokens = u.Output
		out.Usage.TotalTokens = u.Total
	}

	body, _ := json.Marshal(out)

	return body
}

// aggregateChatSSE walks an OpenAI chat SSE stream and concatenates the
// choices[0].delta.content deltas into the full message text, also returning
// the model / id / created from the first chunk that carries them.
func aggregateChatSSE(buf []byte) (content, model, id string, created int64) {
	var sb strings.Builder
	for _, line := range strings.Split(string(buf), "\n") {
		line = strings.TrimRight(strings.TrimSpace(line), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" || payload == "[DONE]" {
			continue
		}

		var chunk struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Created int64  `json:"created"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}

		if model == "" && chunk.Model != "" {
			model = chunk.Model
		}

		if id == "" && chunk.ID != "" {
			id = chunk.ID
		}

		if created == 0 && chunk.Created != 0 {
			created = chunk.Created
		}

		if len(chunk.Choices) > 0 {
			sb.WriteString(chunk.Choices[0].Delta.Content)
		}
	}

	return sb.String(), model, id, created
}

func stripPrefix(s, prefix string) string {
	if len(s) > len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):]
	}

	return s
}
