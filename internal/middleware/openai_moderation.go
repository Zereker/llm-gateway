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

	"github.com/zereker/llm-gateway/internal/domain"
)

// openaiModerationDefaultBaseURL is OpenAI's official moderation endpoint.
const openaiModerationDefaultBaseURL = "https://api.openai.com"

// openaiModerationModel: v0.5 uses omni-moderation-latest (supports text+image,
// free). Change the implementation if you want text-moderation-latest instead.
const openaiModerationModel = "omni-moderation-latest"

// OpenAIModerator calls OpenAI's /v1/moderations API for content moderation.
//
// **CheckInput**: extracts user / system text from rc.Envelope.RawBytes,
// concatenates it, and sends it to OpenAI moderation; any category flagged
// ("hate" / "harassment" / "sexual" / "violence", etc.) → returns an error so
// M8 rejects the request.
//
// **CheckOutput**: the v1.0 decorator architecture is ready (M8 → ctx → M7
// wrapWithModerator), but this Moderator implementation is still a noop —
// chunk-level moderation requires: (a) parsing SSE / joining into continuous
// text (b) accumulating by sentence boundary before calling the OpenAI API
// (c) controlling API QPS to avoid being rate-limited. These will be done in
// separate v1.x tickets; the current stub lets the architecture run end-to-end
// tests.
//
// **HTTP client**: built-in http.Client (Timeout 5s); moderation hits a
// lightweight endpoint, typically < 200ms. The 5s timeout leaves headroom for
// slow networks.
//
// **Concurrent-safe**: the internal http.Client + config never change; safe
// for multiple goroutines.
type OpenAIModerator struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

// NewOpenAIModerator constructs an OpenAI moderation client.
//
// Leave baseURL empty to use OpenAI's official https://api.openai.com
// (production can point to an OpenAI-compatible upstream such as Azure OpenAI,
// but confirm first that its /v1/moderations is compatible).
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

// CheckInput implements Moderator.CheckInput.
//
// Behavior:
//  1. Extracts the text payload from the envelope (messages content + system)
//  2. Calls /v1/moderations
//  3. flagged=true → returns an error listing the flagged categories
//  4. HTTP error (non-200 / network): returns an error; M8 treats it as a
//     client 400 rejection (conservative)
//
// Returning nil = passed moderation.
func (m *OpenAIModerator) CheckInput(ctx context.Context, env *domain.RequestEnvelope) error {
	text := extractTextForModeration(env)
	if text == "" {
		// No text to moderate (pure tool call / empty messages) → pass
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
	// Collect the flagged categories to show the client
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

// CheckOutput stub: the decorator architecture (internal/middleware/moderation_handler.go)
// calls this method with chunk bytes, but this implementation currently just
// returns nil to pass through — actually doing it requires SSE parsing +
// sentence accumulation + API rate limiting, left for a separate v1.x ticket.
//
// A custom Moderator implementation wanting chunk-level moderation can work
// off the chunk bytes (note: the chunk is the bytes **the client actually
// sees** after translator translation, not the raw upstream chunk).
func (m *OpenAIModerator) CheckOutput(_ context.Context, _ []byte) error {
	return nil
}

// extractTextForModeration extracts the text to moderate from the envelope.
//
// **Extraction strategy** (v0.5 simplified):
//   - Uses RawBytes (Envelope.Parsed.Messages is json.RawMessage; parsing
//     another layer isn't worth it)
//   - Grabs the messages[].content field (string or array of text blocks)
//   - Grabs system / systemInstruction
//
// An empty string means there's no text to moderate.
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

// decodeStringField: the content / system field may be a string or a
// {parts:[{text}]} shape; try both.
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

// decodeContentField: OpenAI/Anthropic's message.content may be a string or
// an array of blocks.
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

// truncate truncates a string to max length (used for error messages to
// prevent large upstream HTML from flooding the logs).
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// Compile-time assertion.
var _ Moderator = (*OpenAIModerator)(nil)
