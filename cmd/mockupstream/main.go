// Command mockupstream is a fake upstream for E2E tests: it emulates the
// OpenAI Chat Completions / Anthropic Messages protocols with fixed
// responses that include usage fields, so the gateway M10 + Flink billing
// aggregation pipeline can be exercised without depending on real
// OpenAI/Anthropic keys.
//
// Routes:
//
//	POST /v1/chat/completions  → OpenAI Chat (includes usage{prompt_tokens, completion_tokens, total_tokens})
//	POST /v1/messages          → Anthropic Messages (includes usage{input_tokens, output_tokens})
//	POST /v1beta/models/{model}:generateContent  → Gemini generateContent
//	GET  /health               → "ok"
//
// Streaming (when stream=true in the request body) emits chunks in each
// protocol's SSE format, followed by a trailing usage chunk.
//
// Listen address: MOCK_ADDR (default :9090).
package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	addr := os.Getenv("MOCK_ADDR")
	if addr == "" {
		addr = ":9090"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/v1/chat/completions", handleOpenAIChat)
	mux.HandleFunc("/v1/messages", handleAnthropicMessages)
	mux.HandleFunc("/v1beta/models/", handleGemini)

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
// helpers
// =============================================================================

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func writeSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
}
