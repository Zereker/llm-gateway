// Command mockupstream is a fake upstream for E2E tests: it emulates the
// OpenAI Chat Completions / Anthropic Messages / Gemini / Cohere / Bedrock
// protocols, so the gateway M10 + Flink billing aggregation pipeline can be
// exercised without depending on real vendor keys.
//
// At startup it loads the same testdata/fieldmatrix/endpoints manifests the
// gateway seeds from (see loadRecordedReplies) and, keyed by upstream model,
// replays each manifest's recorded response — opencassette / vendor-cassettes /
// fixture, the exact data the in-process e2e (internal/app/gateway) validates —
// so the real-binary smoke test exercises real captured vendor traffic. A model
// with no manifest entry falls back to a canned per-protocol response (with
// usage fields), so the mock still runs standalone for ad-hoc debugging.
//
// Routes:
//
//	POST /v1/chat/completions  → OpenAI Chat (includes usage{prompt_tokens, completion_tokens, total_tokens})
//	POST /v1/messages          → Anthropic Messages (includes usage{input_tokens, output_tokens})
//	POST /v1beta/models/{model}:generateContent  → Gemini generateContent
//	POST /v2/chat              → Cohere v2/chat (includes usage.tokens{input_tokens,output_tokens})
//	POST /openai/deployments/{deployment}/chat/completions  → Azure OpenAI (identical wire shape to
//	     plain OpenAI Chat -- reuses handleOpenAIChat; Azure-specific bits are the URL shape and the
//	     api-key header, both handled entirely on the gateway side by internal/protocol/azureopenai)
//	POST /model/{modelId}/converse  → Bedrock Converse (includes usage{inputTokens,outputTokens,totalTokens})
//	GET  /health               → "ok"
//
// Streaming (when stream=true in the request body) emits chunks in each
// protocol's SSE format, followed by a trailing usage chunk. Bedrock
// Converse is the one exception -- no /converse-stream handler is
// implemented since nothing in this repo's smoke tests sends stream:true to
// it yet (a real Converse stream is AWS event-stream binary framing, not
// SSE, so a mock for it isn't a small addition -- add it if/when a test
// actually needs it).
//
// Listen address: MOCK_ADDR (default :9090).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/zereker/llm-gateway/internal/cassette"
	"github.com/zereker/llm-gateway/internal/cassette/vendorfixture"
)

// recordedReplies maps an upstream model name to the raw recorded response
// body this mock returns for it, loaded once at startup from the same
// testdata/fieldmatrix/endpoints manifests the gateway seeds from (see
// loadRecordedReplies). This makes the real-binary smoke test replay the exact
// captured vendor data — opencassette / vendor-cassettes / fixture — that the
// in-process e2e (internal/app/gateway) already validates, instead of a canned
// per-protocol stub. A model with no manifest entry falls back to the canned
// response below, so the mock still runs standalone for ad-hoc debugging.
var recordedReplies map[string][]byte

// loadRecordedReplies builds the model -> recorded-body map from the endpoint
// manifests. A manifest that can't be loaded or a reply that can't be resolved
// is logged and skipped (that model then gets the canned response) rather than
// aborting startup — the mock must still come up for the health check.
func loadRecordedReplies() map[string][]byte {
	m := map[string][]byte{}

	scenarios, err := vendorfixture.LoadDir(cassette.TestdataPath("fieldmatrix", "endpoints"))
	if err != nil {
		slog.Warn("mockupstream: manifests not loaded; serving canned responses only", "err", err)
		return m
	}

	for _, sc := range scenarios {
		body, err := vendorfixture.ResolveReply(sc.Reply)
		if err != nil {
			slog.Warn("mockupstream: reply unresolved; model gets canned response",
				"vendor", sc.Vendor, "model", sc.Model, "err", err)
			continue
		}
		m[sc.Model] = body
	}

	slog.Info("mockupstream: recorded replies loaded", "models", len(m))
	return m
}

// serveRecorded writes the recorded reply for model and returns true when one
// exists; otherwise it returns false so the caller falls back to its canned
// body. The Content-Type mirrors the recorded body's shape (SSE vs JSON), the
// same sniff the in-process mock upstream uses.
func serveRecorded(w http.ResponseWriter, model string) bool {
	if model == "" {
		return false
	}
	body, ok := recordedReplies[model]
	if !ok {
		return false
	}

	trimmed := bytes.TrimSpace(body)
	if bytes.HasPrefix(trimmed, []byte("event:")) || bytes.HasPrefix(trimmed, []byte("data:")) {
		writeSSEHeaders(w)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	_, _ = w.Write(body)
	return true
}

func main() {
	addr := os.Getenv("MOCK_ADDR")
	if addr == "" {
		addr = ":9090"
	}

	recordedReplies = loadRecordedReplies()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/v1/chat/completions", handleOpenAIChat)
	mux.HandleFunc("/v1/messages", handleAnthropicMessages)
	mux.HandleFunc("/v1beta/models/", handleGemini)
	mux.HandleFunc("/v2/chat", handleCohereChat)
	mux.HandleFunc("/openai/deployments/", handleOpenAIChat)
	mux.HandleFunc("/model/", handleBedrockConverse)

	slog.Info("mockupstream listening", "addr", addr)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
}

// =============================================================================
// OpenAI Chat Completions
// =============================================================================

type openAIRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"messages"`
}

func handleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	var req openAIRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), 400)
		return
	}

	if serveRecorded(w, req.Model) {
		return
	}

	model := req.Model
	if model == "" {
		model = "gpt-4o"
	}

	const (
		prompt     = 12
		completion = 8
	)

	if req.Stream {
		writeSSEHeaders(w)
		flusher, _ := w.(http.Flusher)
		writeChunk := func(payload any) {
			b, _ := json.Marshal(payload)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)

			if flusher != nil {
				flusher.Flush()
			}
		}
		writeChunk(map[string]any{
			"id":      "chatcmpl-mock",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]any{"role": "assistant", "content": "Hello "},
			}},
		})
		writeChunk(map[string]any{
			"id":      "chatcmpl-mock",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]any{"content": "from mock."},
				"finish_reason": "stop",
			}},
		})
		writeChunk(map[string]any{
			"id":      "chatcmpl-mock",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]any{},
			"usage": map[string]any{
				"prompt_tokens":     prompt,
				"completion_tokens": completion,
				"total_tokens":      prompt + completion,
			},
		})

		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")

		if flusher != nil {
			flusher.Flush()
		}

		return
	}

	resp := map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "Hello from mock."},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     prompt,
			"completion_tokens": completion,
			"total_tokens":      prompt + completion,
		},
	}
	writeJSON(w, resp)
}

// =============================================================================
// Anthropic Messages
// =============================================================================

type anthropicRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"messages"`
}

func handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	var req anthropicRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), 400)
		return
	}

	if serveRecorded(w, req.Model) {
		return
	}

	model := req.Model
	if model == "" {
		model = "claude-3-5-sonnet-20241022"
	}

	const (
		input  = 14
		output = 7
	)

	if req.Stream {
		writeSSEHeaders(w)
		flusher, _ := w.(http.Flusher)
		write := func(eventType string, payload any) {
			b, _ := json.Marshal(payload)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, b)

			if flusher != nil {
				flusher.Flush()
			}
		}
		write("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            "msg_mock",
				"type":          "message",
				"role":          "assistant",
				"model":         model,
				"content":       []any{},
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         map[string]any{"input_tokens": input, "output_tokens": 1},
			},
		})
		write("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         0,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		write("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "text_delta", "text": "Hello from mock."},
		})
		write("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		write("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": output},
		})
		write("message_stop", map[string]any{"type": "message_stop"})

		return
	}

	resp := map[string]any{
		"id":            "msg_mock",
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"content": []map[string]any{{
			"type": "text",
			"text": "Hello from mock.",
		}},
		"usage": map[string]any{
			"input_tokens":  input,
			"output_tokens": output,
		},
	}
	writeJSON(w, resp)
}

// =============================================================================
// Gemini generateContent
// =============================================================================

func handleGemini(w http.ResponseWriter, r *http.Request) {
	// path looks like /v1beta/models/gemini-1.5-pro:generateContent or :streamGenerateContent
	streaming := strings.Contains(r.URL.Path, ":streamGenerateContent")

	model := "gemini-1.5-pro"
	if i := strings.LastIndex(r.URL.Path, "/"); i >= 0 {
		rest := r.URL.Path[i+1:]
		if j := strings.Index(rest, ":"); j > 0 {
			model = rest[:j]
		}
	}

	if serveRecorded(w, model) {
		return
	}

	const (
		prompt     = 10
		candidates = 6
	)

	body := map[string]any{
		"candidates": []map[string]any{{
			"content": map[string]any{
				"role": "model",
				"parts": []map[string]any{{
					"text": "Hello from mock.",
				}},
			},
			"finishReason": "STOP",
			"index":        0,
		}},
		"usageMetadata": map[string]any{
			"promptTokenCount":     prompt,
			"candidatesTokenCount": candidates,
			"totalTokenCount":      prompt + candidates,
		},
		"modelVersion": model,
	}

	if streaming {
		writeSSEHeaders(w)
		flusher, _ := w.(http.Flusher)
		b, _ := json.Marshal(body)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)

		if flusher != nil {
			flusher.Flush()
		}

		return
	}

	writeJSON(w, body)
}

// =============================================================================
// Cohere v2/chat
// =============================================================================

type cohereRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"messages"`
}

// handleCohereChat replies with the v2/chat response shape openai_cohere's
// translator expects: message.content as an array of {type,text} blocks
// (not a plain string, unlike OpenAI/Anthropic), usage.tokens.{input,output}
// for billing, finish_reason "COMPLETE" (mapped to OpenAI's "stop").
func handleCohereChat(w http.ResponseWriter, r *http.Request) {
	var req cohereRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), 400)
		return
	}

	if serveRecorded(w, req.Model) {
		return
	}

	const (
		input  = 11
		output = 6
	)

	if req.Stream {
		writeSSEHeaders(w)
		flusher, _ := w.(http.Flusher)
		write := func(payload any) {
			b, _ := json.Marshal(payload)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)

			if flusher != nil {
				flusher.Flush()
			}
		}
		write(map[string]any{"type": "message-start", "delta": map[string]any{"message": map[string]any{"role": "assistant"}}})
		write(map[string]any{"type": "content-start", "index": 0, "delta": map[string]any{"message": map[string]any{"content": map[string]any{"type": "text", "text": ""}}}})
		write(map[string]any{"type": "content-delta", "index": 0, "delta": map[string]any{"message": map[string]any{"content": map[string]any{"text": "Hello from mock."}}}})
		write(map[string]any{"type": "content-end", "index": 0})
		write(map[string]any{
			"type": "message-end",
			"delta": map[string]any{
				"finish_reason": "COMPLETE",
				"usage": map[string]any{
					"billed_units": map[string]any{"input_tokens": input, "output_tokens": output},
					"tokens":       map[string]any{"input_tokens": input, "output_tokens": output},
				},
			},
		})

		return
	}

	resp := map[string]any{
		"id": "mock-cohere-id",
		"message": map[string]any{
			"role":    "assistant",
			"content": []map[string]any{{"type": "text", "text": "Hello from mock."}},
		},
		"finish_reason": "COMPLETE",
		"usage": map[string]any{
			"billed_units": map[string]any{"input_tokens": input, "output_tokens": output},
			"tokens":       map[string]any{"input_tokens": input, "output_tokens": output},
		},
	}
	writeJSON(w, resp)
}

// =============================================================================
// Bedrock Converse
// =============================================================================

type converseRequest struct {
	Messages []struct {
		Role    string `json:"role"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"messages"`
}

// handleBedrockConverse replies with the non-streaming Converse response
// shape internal/translator/openai_bedrock expects: output.message.content
// as an array of {text} blocks, usage.{inputTokens,outputTokens,totalTokens}.
// No signature verification (SigV4) -- the mock accepts any request that
// parses as JSON.
func handleBedrockConverse(w http.ResponseWriter, r *http.Request) {
	if serveRecorded(w, bedrockModelFromPath(r.URL.Path)) {
		return
	}

	var req converseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), 400)
		return
	}

	const (
		input  = 9
		output = 5
	)

	resp := map[string]any{
		"output": map[string]any{
			"message": map[string]any{
				"role":    "assistant",
				"content": []map[string]any{{"text": "Hello from mock."}},
			},
		},
		"stopReason": "end_turn",
		"usage": map[string]any{
			"inputTokens": input, "outputTokens": output, "totalTokens": input + output,
		},
	}
	writeJSON(w, resp)
}

// =============================================================================
// helpers
// =============================================================================

// bedrockModelFromPath extracts the model id from a Converse request path of
// the form /model/<modelId>/converse (or /converse-stream), so a recorded
// reply can be keyed by the same model the manifest seeds.
func bedrockModelFromPath(p string) string {
	const prefix = "/model/"
	i := strings.Index(p, prefix)
	if i < 0 {
		return ""
	}
	rest := p[i+len(prefix):]
	if j := strings.Index(rest, "/"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func writeSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
}
