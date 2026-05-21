// Package openai_gemini OpenAI 客户端 → Gemini 上游的 Translator。
//
// 客户端按 OpenAI ChatCompletion 格式发请求；本 translator 翻成 Gemini generateContent
// 格式给 adapter 转发；上游响应再翻回 OpenAI 格式给客户端。
//
// **v0.5 限制**：
//   - 只支持 chat（system/user/assistant/text content）
//   - 不支持 streaming（buffer-then-translate at Flush；Gemini stream 格式跟 OpenAI SSE 不同，
//     单独迭代加流式翻译）
//   - 不支持 function calling / tool_use / vision
//
// 字段映射详见 translateRequest / translateResponse。
package openai_gemini

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/translator"
	"github.com/zereker/llm-gateway/pkg/usage/extractor"
)

type openaiGemini struct{}

// Translator (OpenAI → Gemini) 公共构造器——给 pkg/protocol/gemini 用。
func Translator() translator.Translator { return openaiGemini{} }

func (openaiGemini) Source() domain.Protocol { return domain.ProtoOpenAI }
func (openaiGemini) Target() domain.Protocol { return domain.ProtoGemini }

func (openaiGemini) TranslateRequest(srcBody []byte) ([]byte, error) {
	return translateRequest(srcBody)
}

func (openaiGemini) NewResponseHandler() translator.ResponseHandler {
	return &responseHandler{ex: extractor.NewGemini()}
}

// responseHandler buffer-then-translate 模式：Feed 全部累积，Flush 一次性翻译。
//
// v0.6 加流式翻译时改这里：解析 Gemini chunk JSON 并实时翻成 OpenAI SSE chunk。
//
// usage 走 extractor.Gemini 旁路（v0.5 G6 抽出）；translateResponse 不再返 usage。
type responseHandler struct {
	buf          []byte
	requestModel string // 翻回 OpenAI 时填到 response.model；目前留空，客户端能接受
	ex           extractor.Session
}

func (h *responseHandler) Feed(chunk []byte) ([]byte, error) {
	h.buf = append(h.buf, chunk...)
	h.ex.Feed(chunk)
	return nil, nil // buffer 模式：不写客户端
}

func (h *responseHandler) Flush() ([]byte, *domain.Usage, error) {
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
