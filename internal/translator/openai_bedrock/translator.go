// Package openai_bedrock is the Translator from the OpenAI client protocol to
// AWS Bedrock's **Converse** API (as opposed to InvokeModel, which
// internal/protocol/bedrock's existing "protocol: anthropic" path already
// covers by reusing openai_anthropic — Converse has a completely different,
// model-agnostic wire shape and needs its own translator).
//
// **Scope**: verified against real captured cassettes (langchain-ai/langchain-aws,
// MIT, see testdata/vendor-cassettes/bedrock/langchain-ai-langchain-aws/) —
// specifically the Claude-on-Bedrock-via-Converse cassettes. Converse is
// designed to be model-family-agnostic (Titan/Nova/Llama/...), but this
// translator has only been verified against Claude traffic; other model
// families may emit fields this translator doesn't yet handle.
//
// **Not translated** (real cassette data exists for these but no
// OpenAI-compatible mapping has been designed yet — same precedent as
// openai_cohere's citations/tool_plan): Converse's `citationsContent` (RAG
// source grounding) and the request-side replay of a prior turn's
// `reasoningContent` (Bedrock's signed extended-thinking blocks — the
// signature must round-trip verbatim on the next turn, which isn't tracked
// yet). Response-side reasoningContent IS surfaced as `reasoning_content`
// (matching the openai_anthropic / openai_cohere convention), just not sent
// back on the next turn's request.
//
// **Request shape** (OpenAI ChatCompletion -> Converse):
//   - messages[].content is always an array of typed blocks ({"text":...},
//     {"toolUse":{...}}, {"toolResult":{...}}), never a bare string.
//   - System messages collect into a top-level "system": [{"text":...}] array
//     (Converse has no "system" role inside messages).
//   - Tool-role messages (OpenAI's tool_call_id + result content) become a
//     "toolResult" content block on a **user**-role message — Converse
//     requires tool results to arrive as part of the next user turn, not a
//     distinct role, so consecutive OpenAI tool messages are merged into one
//     Converse user message (multiple toolResult blocks), matching what a real
//     multi-tool-call turn looks like on the wire.
//   - A synthetic top-level "stream" bool is included so
//     internal/protocol/bedrock's Converse session can pick /converse vs
//     /converse-stream without re-deriving it from the client body; the
//     session strips it before signing/sending (see toConverseHTTPBody).
//
// **Response shape** (Converse -> OpenAI ChatCompletion):
//   - Non-streaming: a single JSON object, output.message.content[] (text /
//     toolUse blocks), usage.{inputTokens,outputTokens,totalTokens}.
//   - Streaming: NOT Converse's raw wire format — internal/protocol/bedrock's
//     transport decoder (see its doc comment) re-frames the AWS event-stream
//     into `event: <type>\ndata: <json>\n\n` lines (mirroring Anthropic's own
//     SSE convention), since Converse's event *type* lives in an AWS
//     event-stream frame header, not inside the JSON payload itself. This
//     translator's streaming parser expects exactly that shape.
package openai_bedrock

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/translator"
)

// Wire vocabulary this file both reads (switch/case on an upstream field) and
// writes (as a literal in a constructed map[string]any) — named once per
// value so a typo shows up as a compile error instead of a silently wrong
// JSON key/value.
const (
	roleAssistant = "assistant"
	roleUser      = "user"

	keyType      = "type"
	keyIndex     = "index"
	keyName      = "name"
	keyFunction  = "function"
	keyToolCalls = "tool_calls"
	keyArguments = "arguments"

	finishStop          = "stop"
	finishContentFilter = "content_filter"
)

type openaiBedrock struct{}

// New returns the OpenAI-to-Bedrock-Converse translator.
func New() translator.Translator { return openaiBedrock{} }

func (openaiBedrock) Source() domain.Protocol { return domain.ProtoOpenAI }
func (openaiBedrock) Target() domain.Protocol { return domain.ProtoBedrock }

func (openaiBedrock) TranslateRequest(srcBody []byte) ([]byte, error) {
	return translateRequest(srcBody)
}

func (openaiBedrock) NewResponseHandler() translator.ResponseHandler {
	return &responseHandler{}
}

// =============================================================================
// Request: OpenAI ChatCompletion -> Bedrock Converse
// =============================================================================

type openaiReq struct {
	Model       string          `json:"model"`
	Messages    []openaiMsg     `json:"messages"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Stop        json.RawMessage `json:"stop,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	Tools       json.RawMessage `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
}

type openaiMsg struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"`
	ToolCalls  []openaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type converseReq struct {
	Messages        []converseMsg    `json:"messages"`
	System          []converseText   `json:"system,omitempty"`
	InferenceConfig *inferenceConfig `json:"inferenceConfig,omitempty"`
	ToolConfig      *toolConfig      `json:"toolConfig,omitempty"`
	Stream          bool             `json:"stream"` // synthetic; stripped by the Converse session before sending
}

type converseMsg struct {
	Role    string        `json:"role"`
	Content []converseBlk `json:"content"`
}

// converseBlk is a tagged union: exactly one of these fields is set per
// block, matching Converse's own untagged-object-per-kind wire shape.
type converseBlk struct {
	Text       string           `json:"text,omitempty"`
	ToolUse    *converseToolUse `json:"toolUse,omitempty"`
	ToolResult *converseToolRes `json:"toolResult,omitempty"`
}

type converseText struct {
	Text string `json:"text"`
}

type converseToolUse struct {
	ToolUseID string          `json:"toolUseId"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

type converseToolRes struct {
	ToolUseID string         `json:"toolUseId"`
	Content   []converseText `json:"content"`
	Status    string         `json:"status,omitempty"`
}

type inferenceConfig struct {
	MaxTokens     *int     `json:"maxTokens,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"topP,omitempty"`
	StopSequences []string `json:"stopSequences,omitempty"`
}

type toolConfig struct {
	Tools      []converseTool  `json:"tools"`
	ToolChoice json.RawMessage `json:"toolChoice,omitempty"`
}

type converseTool struct {
	ToolSpec converseToolSpec `json:"toolSpec"`
}

type converseToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema converseSchema `json:"inputSchema"`
}

type converseSchema struct {
	JSON json.RawMessage `json:"json"`
}

func translateRequest(srcBody []byte) ([]byte, error) {
	var in openaiReq
	if err := json.Unmarshal(srcBody, &in); err != nil {
		return nil, err
	}

	out := converseReq{Stream: in.Stream}
	if in.MaxTokens != nil || in.Temperature != nil || in.TopP != nil || len(in.Stop) > 0 {
		ic := &inferenceConfig{MaxTokens: in.MaxTokens, Temperature: in.Temperature, TopP: in.TopP}
		ic.StopSequences = parseStopField(in.Stop)
		out.InferenceConfig = ic
	}

	if len(in.Tools) > 0 {
		if tc := buildToolConfig(in.Tools, in.ToolChoice); tc != nil {
			out.ToolConfig = tc
		}
	}

	// Consecutive OpenAI "tool" messages merge into one Converse user message
	// (multiple toolResult blocks) -- Converse requires tool results to arrive
	// as part of the next user turn, not a distinct role, and a real
	// multi-tool-call turn's answer comes back as several consecutive "tool"
	// messages in the OpenAI history.
	for _, m := range in.Messages {
		switch m.Role {
		case "system":
			out.System = append(out.System, converseText{Text: contentToString(m.Content)})
		case roleAssistant:
			blk := converseMsg{Role: roleAssistant}
			if text := contentToString(m.Content); text != "" {
				blk.Content = append(blk.Content, converseBlk{Text: text})
			}

			for _, tc := range m.ToolCalls {
				input := json.RawMessage(tc.Function.Arguments)
				if !json.Valid(input) {
					input = json.RawMessage("{}")
				}

				blk.Content = append(blk.Content, converseBlk{ToolUse: &converseToolUse{
					ToolUseID: tc.ID, Name: tc.Function.Name, Input: input,
				}})
			}

			out.Messages = append(out.Messages, blk)
		case "tool":
			res := converseBlk{ToolResult: &converseToolRes{
				ToolUseID: m.ToolCallID,
				Content:   []converseText{{Text: contentToString(m.Content)}},
				Status:    "success",
			}}
			if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == roleUser && isToolResultOnly(out.Messages[n-1]) {
				out.Messages[n-1].Content = append(out.Messages[n-1].Content, res)
			} else {
				out.Messages = append(out.Messages, converseMsg{Role: roleUser, Content: []converseBlk{res}})
			}
		default: // user
			out.Messages = append(out.Messages, converseMsg{Role: roleUser, Content: []converseBlk{{Text: contentToString(m.Content)}}})
		}
	}

	return json.Marshal(out)
}

// isToolResultOnly reports whether every block in m is a toolResult (so a new
// consecutive "tool" message should append to it rather than start a new
// Converse message).
func isToolResultOnly(m converseMsg) bool {
	for _, b := range m.Content {
		if b.ToolResult == nil {
			return false
		}
	}

	return len(m.Content) > 0
}

// buildToolConfig converts OpenAI tools[] ({type:"function",function:{name,
// description,parameters}}) into Converse's toolConfig.tools[].toolSpec — a
// field rename/reshape (parameters -> inputSchema.json), not a semantic
// change. toolChoice maps OpenAI's four shapes onto Converse's three
// ({"auto":{}}/{"any":{}}/{"tool":{"name":...}}); "none" has no Converse
// equivalent when tools are present, so it's dropped (falls back to
// Converse's default, which lets the model decide) rather than guessed at.
func buildToolConfig(rawTools json.RawMessage, rawChoice json.RawMessage) *toolConfig {
	var tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(rawTools, &tools); err != nil || len(tools) == 0 {
		return nil
	}

	tc := &toolConfig{Tools: make([]converseTool, 0, len(tools))}
	for _, t := range tools {
		params := t.Function.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}

		tc.Tools = append(tc.Tools, converseTool{ToolSpec: converseToolSpec{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: converseSchema{JSON: params},
		}})
	}

	if len(rawChoice) > 0 {
		tc.ToolChoice = mapToolChoice(rawChoice)
	}

	return tc
}

func mapToolChoice(raw json.RawMessage) json.RawMessage {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "required":
			return json.RawMessage(`{"any":{}}`)
		case "auto", "none":
			return nil // Converse's default (model decides) is the closest available for both
		}
	}

	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Type == keyFunction && obj.Function.Name != "" {
		b, _ := json.Marshal(map[string]any{"tool": map[string]string{keyName: obj.Function.Name}})
		return b
	}

	return nil
}

func parseStopField(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []string{s}
	}

	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr
	}

	return nil
}

// contentToString flattens OpenAI content (a string or a multimodal array)
// into plain text. Converse has its own image content block shape distinct
// enough from OpenAI's that vision isn't handled by this translator yet —
// like citations/thinking-replay, deferred rather than guessed at.
func contentToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	r := gjson.ParseBytes(raw)
	if r.Type == gjson.String {
		return r.String()
	}

	if r.IsArray() {
		var sb strings.Builder
		r.ForEach(func(_, part gjson.Result) bool {
			if t := part.Get("text"); t.Exists() {
				sb.WriteString(t.String())
			}

			return true
		})

		return sb.String()
	}

	return r.String()
}

// =============================================================================
// Response: Bedrock Converse -> OpenAI ChatCompletion
// =============================================================================

type respMode int

const (
	modeUnknown respMode = iota
	modeJSON
	modeSSE
)

type responseHandler struct {
	mode        respMode
	buf         []byte
	lineBuf     []byte
	id          string
	usage       *domain.Usage
	blockKind   map[int64]string // contentBlockIndex -> "toolUse" (defaults to text otherwise)
	toolNames   map[int64]string // contentBlockIndex -> tool name (from contentBlockStart)
	toolIDs     map[int64]string // contentBlockIndex -> toolUseId (from contentBlockStart)
	pendingType string           // event-type of the "event:" line awaiting its "data:" line
}

func (h *responseHandler) Feed(chunk []byte) ([]byte, error) {
	switch h.mode {
	case modeJSON:
		h.buf = append(h.buf, chunk...)
		return nil, nil
	case modeSSE:
		h.lineBuf = append(h.lineBuf, chunk...)
		return h.drain(), nil
	default:
		h.buf = append(h.buf, chunk...)

		t := bytes.TrimLeft(h.buf, " \t\r\n")
		if len(t) == 0 {
			return nil, nil
		}

		if t[0] == '{' {
			h.mode = modeJSON
			return nil, nil
		}

		h.mode = modeSSE
		h.lineBuf = h.buf
		h.buf = nil

		return h.drain(), nil
	}
}

func (h *responseHandler) Flush() ([]byte, *domain.Usage, error) {
	if h.mode == modeSSE {
		out := h.drain()
		if rest := bytes.TrimSpace(h.lineBuf); len(rest) > 0 {
			h.lineBuf = nil
			out = append(out, h.consumeLine(rest)...)
		}

		out = append(out, "data: [DONE]\n\n"...)

		return out, h.usage, nil
	}

	if len(h.buf) == 0 {
		return nil, nil, nil
	}
	// Error responses have no "output" field; pass them through as-is so the
	// caller sees the real upstream error rather than a translation failure.
	if !gjson.GetBytes(h.buf, "output").Exists() {
		return h.buf, nil, nil
	}

	body, usage := translateResponse(h.buf)

	return body, usage, nil
}

// drain extracts complete lines from lineBuf, pairing each "event:" line with
// its following "data:" line (mirroring internal/protocol/bedrock's
// transport-decoder framing — see this package's doc comment).
func (h *responseHandler) drain() []byte {
	var out []byte
	for {
		i := bytes.IndexByte(h.lineBuf, '\n')
		if i < 0 {
			break
		}

		line := bytes.TrimRight(h.lineBuf[:i], "\r")
		h.lineBuf = h.lineBuf[i+1:]
		out = append(out, h.consumeLine(line)...)
	}

	return out
}

func (h *responseHandler) consumeLine(line []byte) []byte {
	switch {
	case bytes.HasPrefix(line, []byte("event:")):
		h.pendingType = string(bytes.TrimSpace(line[len("event:"):]))
		return nil
	case bytes.HasPrefix(line, []byte("data:")):
		data := bytes.TrimSpace(line[len("data:"):])
		if len(data) == 0 {
			return nil
		}

		out := h.translateEvent(h.pendingType, data)
		h.pendingType = ""

		return out
	default:
		return nil
	}
}

func (h *responseHandler) translateEvent(eventType string, data []byte) []byte {
	ev := gjson.ParseBytes(data)
	switch eventType {
	case "messageStart":
		return h.chunk(map[string]any{"role": roleAssistant}, "")
	case "contentBlockStart":
		idx := ev.Get("contentBlockIndex").Int()
		if tu := ev.Get("start.toolUse"); tu.Exists() {
			if h.blockKind == nil {
				h.blockKind, h.toolNames, h.toolIDs = map[int64]string{}, map[int64]string{}, map[int64]string{}
			}

			h.blockKind[idx] = "toolUse"
			h.toolNames[idx] = tu.Get(keyName).String()
			h.toolIDs[idx] = tu.Get("toolUseId").String()

			return h.chunk(map[string]any{keyToolCalls: []any{map[string]any{
				keyIndex: idx, "id": h.toolIDs[idx], keyType: keyFunction,
				keyFunction: map[string]any{keyName: h.toolNames[idx], keyArguments: ""},
			}}}, "")
		}

		return nil
	case "contentBlockDelta":
		idx := ev.Get("contentBlockIndex").Int()
		if h.blockKind[idx] == "toolUse" {
			frag := ev.Get("delta.toolUse.input").String()
			if frag == "" {
				return nil
			}

			return h.chunk(map[string]any{keyToolCalls: []any{map[string]any{
				keyIndex: idx, keyFunction: map[string]any{keyArguments: frag},
			}}}, "")
		}

		if reasoning := ev.Get("delta.reasoningContent.text").String(); reasoning != "" {
			return h.chunk(map[string]any{"reasoning_content": reasoning}, "")
		}

		text := ev.Get("delta.text").String()
		if text == "" {
			return nil
		}

		return h.chunk(map[string]any{"content": text}, "")
	case "contentBlockStop":
		return nil
	case "messageStop":
		return h.chunk(map[string]any{}, mapFinishReason(ev.Get("stopReason").String()))
	case "metadata":
		usageResult := ev.Get("usage")
		in := usageResult.Get("inputTokens").Int()
		outTok := usageResult.Get("outputTokens").Int()

		var rawUsage []byte
		if usageResult.Exists() {
			rawUsage = []byte(usageResult.Raw)
		}

		h.usage = &domain.Usage{Input: in, Output: outTok, Total: in + outTok, Raw: rawUsage, Source: domain.UsageSourceUpstream, Confidence: domain.UsageConfidenceExact}

		return nil
	default: // e.g. exception frames the transport layer passed through as-is
		return nil
	}
}

func (h *responseHandler) chunk(delta map[string]any, finish string) []byte {
	if h.id == "" {
		h.id = "chatcmpl-" + randHex(8)
	}

	choice := map[string]any{keyIndex: 0, "delta": delta, "finish_reason": nil}
	if finish != "" {
		choice["finish_reason"] = finish
	}

	b, _ := json.Marshal(map[string]any{
		"id": h.id, "object": "chat.completion.chunk", "choices": []any{choice},
	})

	return append(append([]byte("data: "), b...), '\n', '\n')
}

func translateResponse(buf []byte) ([]byte, *domain.Usage) {
	root := gjson.ParseBytes(buf)

	var (
		text, reasoning strings.Builder
		toolCalls       []map[string]any
	)
	root.Get("output.message.content").ForEach(func(_, part gjson.Result) bool {
		switch {
		case part.Get("text").Exists():
			text.WriteString(part.Get("text").String())
		case part.Get("toolUse").Exists():
			tu := part.Get("toolUse")
			toolCalls = append(toolCalls, map[string]any{
				"id":    tu.Get("toolUseId").String(),
				keyType: keyFunction,
				keyFunction: map[string]any{
					keyName:      tu.Get(keyName).String(),
					keyArguments: tu.Get("input").Raw,
				},
			})
		case part.Get("reasoningContent").Exists():
			reasoning.WriteString(part.Get("reasoningContent.reasoningText.text").String())
			// citationsContent: no OpenAI-compatible mapping designed yet (see
			// package doc comment) -- intentionally not handled here.
		}

		return true
	})

	usageResult := root.Get("usage")
	in := usageResult.Get("inputTokens").Int()
	outTok := usageResult.Get("outputTokens").Int()

	var rawUsage []byte
	if usageResult.Exists() {
		rawUsage = []byte(usageResult.Raw)
	}

	usage := &domain.Usage{
		Input: in, Output: outTok, Total: in + outTok, Raw: rawUsage,
		Source: domain.UsageSourceUpstream, Confidence: domain.UsageConfidenceExact,
	}

	message := map[string]any{"role": roleAssistant}
	if len(toolCalls) > 0 {
		message[keyToolCalls] = toolCalls
		if text.Len() > 0 {
			message["content"] = text.String()
		} else {
			message["content"] = nil
		}
	} else {
		message["content"] = text.String()
	}

	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}

	resp := map[string]any{
		"id":     "chatcmpl-" + randHex(8),
		"object": "chat.completion",
		"choices": []any{map[string]any{
			keyIndex:        0,
			"message":       message,
			"finish_reason": mapFinishReason(root.Get("stopReason").String()),
		}},
		"usage": map[string]any{
			"prompt_tokens":     in,
			"completion_tokens": outTok,
			"total_tokens":      in + outTok,
		},
	}
	body, _ := json.Marshal(resp)

	return body, usage
}

// mapFinishReason maps every documented Converse stopReason value
// (end_turn/tool_use/max_tokens/stop_sequence/content_filtered/guardrail_intervened)
// onto OpenAI's smaller enum explicitly, rather than falling back to a lossy
// string transform.
func mapFinishReason(sr string) string {
	switch sr {
	case "end_turn", "stop_sequence", "":
		return finishStop
	case "tool_use":
		return keyToolCalls
	case "max_tokens":
		return "length"
	case "content_filtered", "guardrail_intervened":
		return finishContentFilter
	default:
		return finishStop
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)

	return hex.EncodeToString(b)
}
