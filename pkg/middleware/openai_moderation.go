package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// openaiModerationDefaultBaseURL OpenAI 官方 moderation endpoint。
const openaiModerationDefaultBaseURL = "https://api.openai.com"

// openaiModerationModel v0.5 用 omni-moderation-latest（支持 text+image，免费）。
// 想换 text-moderation-latest 自己改实现。
const openaiModerationModel = "omni-moderation-latest"

// OpenAIModerator 调 OpenAI /v1/moderations 接口做内容审核。
//
// **CheckInput**：从 rc.Envelope.Parsed.Messages 提取 user / system 文本拼起来发给
// OpenAI moderation；任一类别命中（"hate" / "harassment" / "sexual" / "violence" 等）
// → 返 error 让 M8 拒绝请求。
//
// **CheckOutput**：v0.5 不支持流式审核——直接 nil（即不审核 output）。生产里需要审核
// 输出的话 v0.6 嵌进 translator.ResponseHandler.Feed 再做。
//
// **HTTP client**：内置 http.Client（Timeout 5s）；moderation 走轻量 endpoint，
// 一般 < 200ms。timeout 5s 给慢网络留余地。
//
// **Concurrent-safe**：内部 http.Client + 配置不变；多 goroutine 安全。
type OpenAIModerator struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

// NewOpenAIModerator 构造一个 OpenAI moderation client。
//
// baseURL 留空走 OpenAI 官方 https://api.openai.com（生产可指 Azure OpenAI 之类
// OpenAI-compat 上游，但要先确认对方 /v1/moderations 兼容）。
func NewOpenAIModerator(apiKey, baseURL string) *OpenAIModerator {
	if baseURL == "" {
		baseURL = openaiModerationDefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &OpenAIModerator{
		apiKey:  apiKey,
		baseURL: baseURL,
		client:  &http.Client{Timeout: 5 * time.Second},
	}
}

type openaiModerationRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type openaiModerationResponse struct {
	Results []struct {
		Flagged    bool            `json:"flagged"`
		Categories map[string]bool `json:"categories"`
	} `json:"results"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// CheckInput 实现 Moderator.CheckInput。
//
// 行为：
//  1. 从 envelope 抽 text payload（messages content + system）
//  2. 调 /v1/moderations
//  3. flagged=true → return error 列出命中的 categories
//  4. HTTP 错（非 200 / network）：return error；M8 把它当客户端 400 拒（保守）
//
// 返 nil = 通过审核。
func (m *OpenAIModerator) CheckInput(ctx context.Context, env *domain.RequestEnvelope) error {
	text := extractTextForModeration(env)
	if text == "" {
		// 没文本可审（纯工具调用 / 空 messages）→ 通过
		return nil
	}

	reqBody, err := json.Marshal(openaiModerationRequest{
		Model: openaiModerationModel,
		Input: text,
	})
	if err != nil {
		return fmt.Errorf("openai moderation: marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		m.baseURL+"/v1/moderations", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("openai moderation: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+m.apiKey)

	resp, err := m.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("openai moderation: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("openai moderation: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("openai moderation: HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var modResp openaiModerationResponse
	if err := json.Unmarshal(body, &modResp); err != nil {
		return fmt.Errorf("openai moderation: parse response: %w", err)
	}
	if modResp.Error != nil {
		return fmt.Errorf("openai moderation: %s (%s)", modResp.Error.Message, modResp.Error.Type)
	}
	if len(modResp.Results) == 0 {
		return nil
	}
	r := modResp.Results[0]
	if !r.Flagged {
		return nil
	}
	// 收集命中类别给客户端看
	hits := make([]string, 0, 4)
	for cat, flagged := range r.Categories {
		if flagged {
			hits = append(hits, cat)
		}
	}
	if len(hits) == 0 {
		return fmt.Errorf("flagged by moderation")
	}
	return fmt.Errorf("flagged by moderation: %s", strings.Join(hits, ","))
}

// CheckOutput v0.5 不实现流式审核：直接 nil 透过。
//
// 想真审输出：v0.6 把 OpenAI moderation 嵌进 translator.ResponseHandler.Feed，
// 按 token / sentence boundary 累积一段调一次（trade off：延迟 vs 实时拦截）。
func (m *OpenAIModerator) CheckOutput(_ context.Context, _ []byte) error {
	return nil
}

// extractTextForModeration 从 envelope 抽要审的文本。
//
// **抽取策略**（v0.5 简化）：
//   - 走 RawBytes（Envelope.Parsed.Messages 是 json.RawMessage，需要再解一层不划算）
//   - 抓 messages[].content 字段（string 或 array of text blocks）
//   - 抓 system / systemInstruction
//
// 返空字符串说明无可审文本。
func extractTextForModeration(env *domain.RequestEnvelope) string {
	if env == nil || len(env.RawBytes) == 0 {
		return ""
	}
	var probe struct {
		System            json.RawMessage   `json:"system"`
		SystemInstruction json.RawMessage   `json:"systemInstruction"`
		Messages          []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(env.RawBytes, &probe); err != nil {
		return ""
	}

	var b strings.Builder
	if s := decodeStringField(probe.System); s != "" {
		b.WriteString(s)
		b.WriteByte('\n')
	}
	if s := decodeStringField(probe.SystemInstruction); s != "" {
		b.WriteString(s)
		b.WriteByte('\n')
	}
	for _, m := range probe.Messages {
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(m, &msg); err != nil {
			continue
		}
		if s := decodeContentField(msg.Content); s != "" {
			b.WriteString(s)
			b.WriteByte('\n')
		}
	}
	return strings.TrimSpace(b.String())
}

// decodeStringField content / system 字段可能是 string 或 {parts:[{text}]} 形态；都尝试。
func decodeStringField(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Anthropic-style structured system: {parts:[{text:"..."}]}
	var parts struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts.Parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

// decodeContentField OpenAI/Anthropic 的 message.content 可能是 string 或 array of blocks。
func decodeContentField(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if s := decodeStringField(raw); s != "" {
		return s
	}
	// content blocks: [{"type":"text","text":"..."}]
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" || blk.Type == "" {
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return ""
}

// truncate 截断字符串到 max 长度（用于 error message 防止上游大量 HTML 顶进日志）。
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// 编译期断言。
var _ Moderator = (*OpenAIModerator)(nil)
