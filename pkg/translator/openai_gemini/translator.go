// Package openai_gemini OpenAI 客户端 → Gemini 上游的 Translator。
//
// 客户端按 OpenAI ChatCompletion 格式发请求；本 translator 翻成 Gemini generateContent
// 格式给 adapter 转发；上游响应再翻回 OpenAI 格式给客户端。
//
// **支持**：
//   - chat（system/user/assistant/text content）
//   - streaming：客户端 stream:true 时上游走 :streamGenerateContent?alt=sse，
//     responseHandler 增量把 Gemini SSE chunk 翻成 OpenAI SSE chunk（见下）。
//     非流式则 buffer-then-translate（Flush 一次性翻）。
//
// **不支持**：function calling / tool_use / vision（parts 只 text）。
//
// 字段映射详见 translateRequest / translateResponse。
package openai_gemini

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/translator"
	"github.com/zereker/llm-gateway/pkg/usage/extractor"
)

type openaiGemini struct{}

func (openaiGemini) Source() domain.Protocol { return domain.ProtoOpenAI }
func (openaiGemini) Target() domain.Protocol { return domain.ProtoGemini }

func (openaiGemini) TranslateRequest(srcBody []byte) ([]byte, error) {
	return translateRequest(srcBody)
}

func (openaiGemini) NewResponseHandler() translator.ResponseHandler {
	return &responseHandler{ex: extractor.NewGemini()}
}

// responseHandler 自适应上游格式：
//   - JSON（非流式）：buffer-then-translate，Flush 一次性翻成 OpenAI ChatCompletion。
//   - SSE（流式，:streamGenerateContent?alt=sse）：增量把 Gemini SSE chunk 翻成
//     OpenAI chat.completion.chunk SSE。
//
// 首个非空字节嗅探：'{' → JSON；否则 → SSE。usage：JSON 模式走 extractor.Gemini
// 旁路；SSE 模式从末 chunk 的 usageMetadata 直接抽（extractor 不解 SSE）。
type respMode int

const (
	modeUnknown respMode = iota
	modeJSON
	modeSSE
)

type responseHandler struct {
	requestModel string // 翻回 OpenAI 时填到 response.model
	ex           extractor.Session

	mode     respMode
	buf      []byte // JSON 模式累积 / 未定模式暂存
	lineBuf  []byte // SSE 模式行缓冲（跨 Feed 保留半行）
	id       string
	roleSent bool // SSE：是否已发过 role delta
	usage    *domain.Usage
}

func (h *responseHandler) Feed(chunk []byte) ([]byte, error) {
	switch h.mode {
	case modeJSON:
		h.buf = append(h.buf, chunk...)
		h.ex.Feed(chunk)
		return nil, nil
	case modeSSE:
		h.lineBuf = append(h.lineBuf, chunk...)
		return h.drainSSE(), nil
	default: // 未定：嗅探首个非空字节
		h.buf = append(h.buf, chunk...)
		t := bytes.TrimLeft(h.buf, " \t\r\n")
		if len(t) == 0 {
			return nil, nil // 还没拿到非空字节
		}
		if t[0] == '{' {
			h.mode = modeJSON
			h.ex.Feed(h.buf) // 把已暂存的喂给 extractor
			return nil, nil
		}
		h.mode = modeSSE
		h.lineBuf = h.buf
		h.buf = nil
		return h.drainSSE(), nil
	}
}

func (h *responseHandler) Flush() ([]byte, *domain.Usage, error) {
	if h.mode == modeSSE {
		out := h.drainSSE()
		// 末帧无结尾换行兜底（上游 abrupt close）：把残留行当最后一行补处理。
		if rest := bytes.TrimSpace(h.lineBuf); len(rest) > 0 {
			h.lineBuf = nil
			if bytes.HasPrefix(rest, []byte("data:")) {
				if data := bytes.TrimSpace(rest[len("data:"):]); len(data) > 0 {
					out = append(out, h.translateChunk(data)...)
				}
			}
		}
		out = append(out, "data: [DONE]\n\n"...) // OpenAI 流终止符
		return out, h.usage, nil
	}
	// JSON / 空
	if len(h.buf) == 0 {
		// 上游空 body（4xx/5xx + 空 body 可能性）；没东西可透；不报错让 M7 静默
		return nil, nil, nil
	}
	if isGeminiError(h.buf) {
		// **error path**：upstream 返了 4xx/5xx + 错误 JSON body。
		// 不翻译（错误 schema 跟成功响应不同）；直接把原 body 透给客户端，保 error visibility。
		// status 码已经是 upstream 的 4xx/5xx（M7 forwarded），客户端看到非 2xx + Gemini 错误 JSON。
		// 唯一遗憾：错误格式不是 OpenAI shape；将来需要可以加 errorTranslate。
		return h.buf, nil, nil
	}
	body, err := translateResponse(h.buf, h.requestModel)
	return body, h.ex.Final(), err
}

// drainSSE 从 lineBuf 抽出完整行，把 Gemini `data:` 事件翻成 OpenAI SSE chunk。
func (h *responseHandler) drainSSE() []byte {
	var out []byte
	for {
		i := bytes.IndexByte(h.lineBuf, '\n')
		if i < 0 {
			break // 半行，留到下次
		}
		line := bytes.TrimRight(h.lineBuf[:i], "\r")
		h.lineBuf = h.lineBuf[i+1:]
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if len(data) == 0 {
			continue
		}
		out = append(out, h.translateChunk(data)...)
	}
	return out
}

// translateChunk 单个 Gemini SSE chunk（一个完整 geminiResponse JSON，含增量
// candidates）→ OpenAI chat.completion.chunk SSE。usageMetadata 只在末 chunk 出现。
func (h *responseHandler) translateChunk(data []byte) []byte {
	ev := gjson.ParseBytes(data)
	if um := ev.Get("usageMetadata"); um.Exists() {
		in := um.Get("promptTokenCount").Int()
		outTok := um.Get("candidatesTokenCount").Int()
		total := um.Get("totalTokenCount").Int()
		if total == 0 {
			total = in + outTok
		}
		h.usage = &domain.Usage{Input: in, Output: outTok, Total: total, Source: domain.UsageSourceUpstream, Confidence: domain.UsageConfidenceExact}
	}
	cand := ev.Get("candidates.0")
	if !cand.Exists() {
		return nil
	}
	var out []byte
	if !h.roleSent {
		h.roleSent = true
		out = append(out, h.chunk(map[string]any{"role": "assistant"}, "")...)
	}
	var text strings.Builder
	cand.Get("content.parts").ForEach(func(_, p gjson.Result) bool {
		text.WriteString(p.Get("text").String())
		return true
	})
	if t := text.String(); t != "" {
		out = append(out, h.chunk(map[string]any{"content": t}, "")...)
	}
	// finishReason 只在末 chunk 非空——非空时才发带 finish_reason 的收尾 chunk。
	if raw := cand.Get("finishReason").String(); raw != "" {
		out = append(out, h.chunk(map[string]any{}, mapFinishReason(raw))...)
	}
	return out
}

// chunk 构造一个 OpenAI chat.completion.chunk 的 SSE 行。
func (h *responseHandler) chunk(delta map[string]any, finish string) []byte {
	if h.id == "" {
		h.id = "chatcmpl-" + randID()
	}
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": nil}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	b, _ := json.Marshal(map[string]any{
		"id": h.id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
		"model": h.requestModel, "choices": []any{choice},
	})
	return append(append([]byte("data: "), b...), '\n', '\n')
}

// =============================================================================
// OpenAI 输入端 shape（最小必要字段）
// =============================================================================

type openAIRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	MaxTokens   *uint32         `json:"max_tokens,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Stop        json.RawMessage `json:"stop,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// =============================================================================
// Gemini 上游端 shape
// =============================================================================

type geminiRequest struct {
	Contents          []geminiContent  `json:"contents"`
	SystemInstruction *geminiContent   `json:"systemInstruction,omitempty"`
	GenerationConfig  *geminiGenConfig `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiGenConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	MaxOutputTokens *uint32  `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

type geminiResponse struct {
	Candidates    []geminiCandidate `json:"candidates"`
	UsageMetadata *geminiUsageMeta  `json:"usageMetadata,omitempty"`
}

type geminiCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason,omitempty"`
	Index        int           `json:"index"`
}

type geminiUsageMeta struct {
	PromptTokenCount     int64 `json:"promptTokenCount"`
	CandidatesTokenCount int64 `json:"candidatesTokenCount"`
	TotalTokenCount      int64 `json:"totalTokenCount"`
}

// =============================================================================
// OpenAI 输出端 shape（翻回客户端）
// =============================================================================

type openAIResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   openAIUsage    `json:"usage"`
}

type openAIChoice struct {
	Index        int           `json:"index"`
	Message      openAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type openAIUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// =============================================================================
// 翻译函数
// =============================================================================

// translateRequest OpenAI body → Gemini body。
//
// 字段映射：
//
//	messages[role=system]           → systemInstruction
//	messages[role=user]             → contents[role=user, parts[].text]
//	messages[role=assistant]        → contents[role=model, parts[].text]
//	max_tokens                      → generationConfig.maxOutputTokens
//	temperature                     → generationConfig.temperature
//	top_p                           → generationConfig.topP
//	stop (string or []string)       → generationConfig.stopSequences []string
//
// 不支持的 role（tool / function）返回 err。
func translateRequest(rawBody []byte) ([]byte, error) {
	var in openAIRequest
	if err := json.Unmarshal(rawBody, &in); err != nil {
		return nil, fmt.Errorf("openai body parse: %w", err)
	}

	out := geminiRequest{}
	for _, m := range in.Messages {
		switch m.Role {
		case "system":
			out.SystemInstruction = &geminiContent{
				Parts: []geminiPart{{Text: m.Content}},
			}
		case "assistant":
			out.Contents = append(out.Contents, geminiContent{
				Role:  "model",
				Parts: []geminiPart{{Text: m.Content}},
			})
		case "user":
			out.Contents = append(out.Contents, geminiContent{
				Role:  "user",
				Parts: []geminiPart{{Text: m.Content}},
			})
		default:
			return nil, fmt.Errorf("unsupported message role %q (v0.5 openai_gemini handles system/user/assistant only)", m.Role)
		}
	}

	hasCfg := false
	cfg := geminiGenConfig{}
	if in.MaxTokens != nil {
		cfg.MaxOutputTokens = in.MaxTokens
		hasCfg = true
	}
	if in.Temperature != nil {
		cfg.Temperature = in.Temperature
		hasCfg = true
	}
	if in.TopP != nil {
		cfg.TopP = in.TopP
		hasCfg = true
	}
	if len(in.Stop) > 0 {
		cfg.StopSequences = parseStopField(in.Stop)
		if len(cfg.StopSequences) > 0 {
			hasCfg = true
		}
	}
	if hasCfg {
		out.GenerationConfig = &cfg
	}

	return json.Marshal(out)
}

// parseStopField OpenAI stop 字段可能是 string 或 []string；都归一成 []string。
func parseStopField(raw json.RawMessage) []string {
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

// translateResponse Gemini body → OpenAI body。
//
// 字段映射：
//
//	candidates[0].content.parts[].text  → choices[0].message.content (concat)
//	candidates[].finishReason           → choices[].finish_reason (lowercase)
//	usageMetadata.promptTokenCount      → usage.prompt_tokens
//	usageMetadata.candidatesTokenCount  → usage.completion_tokens
//	usageMetadata.totalTokenCount       → usage.total_tokens
//
// requestModel 写到 response.model（Gemini 不返回 model 名）。
//
// 返 *domain.Usage 给 caller 的职责已移出（走 extractor.NewGemini 旁路）；本函数
// 只负责把 usageMetadata 翻进 OpenAI body 的 usage 字段（OpenAI 客户端期待的形态）。
func translateResponse(rawBody []byte, requestModel string) ([]byte, error) {
	var in geminiResponse
	if err := json.Unmarshal(rawBody, &in); err != nil {
		return nil, fmt.Errorf("gemini response parse: %w", err)
	}

	out := openAIResponse{
		ID:      "chatcmpl-" + randID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   requestModel,
	}
	for _, cand := range in.Candidates {
		var content string
		if len(cand.Content.Parts) > 0 {
			var b strings.Builder
			for _, p := range cand.Content.Parts {
				b.WriteString(p.Text)
			}
			content = b.String()
		}
		out.Choices = append(out.Choices, openAIChoice{
			Index:        cand.Index,
			Message:      openAIMessage{Role: "assistant", Content: content},
			FinishReason: mapFinishReason(cand.FinishReason),
		})
	}

	if in.UsageMetadata != nil {
		out.Usage = openAIUsage{
			PromptTokens:     in.UsageMetadata.PromptTokenCount,
			CompletionTokens: in.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      in.UsageMetadata.TotalTokenCount,
		}
	}

	body, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("openai response marshal: %w", err)
	}
	return body, nil
}

func mapFinishReason(g string) string {
	switch strings.ToUpper(g) {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION":
		return "content_filter"
	case "":
		return "stop"
	default:
		return "stop"
	}
}

func isGeminiError(body []byte) bool {
	for i := 0; i < len(body) && i < 30; i++ {
		if body[i] == '{' {
			rest := body[i:]
			if len(rest) > 200 {
				rest = rest[:200]
			}
			for j := 0; j+7 <= len(rest); j++ {
				if string(rest[j:j+7]) == `"error"` {
					return true
				}
			}
			return false
		}
	}
	return false
}

func randID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func init() {
	translator.Register(openaiGemini{})
}
