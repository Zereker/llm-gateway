// Command upstream is a deterministic LLM-like server used only by the
// reproducible benchmark. Request content selects fault scenarios so direct
// and gateway paths receive identical inputs.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

type request struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Content string `json:"content"`
	} `json:"messages"`
}

func main() {
	addr := env("BENCHMARK_ADDR", ":9092")
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/v1/chat/completions", serveChat)
	log.Printf("benchmark upstream listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func serveChat(w http.ResponseWriter, r *http.Request) {
	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	prompt := ""
	if len(req.Messages) > 0 {
		prompt = req.Messages[0].Content
	}
	if !req.Stream {
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"bench","object":"chat.completion","model":%q,"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"benchmark response"}}],"usage":{"prompt_tokens":8,"completion_tokens":4,"total_tokens":12}}`, req.Model)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 8; i++ {
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"bench\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"token-%d \"}}]}\n\n", req.Model, i)
		flusher.Flush()
		if strings.Contains(prompt, "mid-stream-failure") && i == 1 {
			if hijacker, ok := w.(http.Hijacker); ok {
				conn, _, err := hijacker.Hijack()
				if err == nil {
					_ = conn.Close()
				}
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	flusher.Flush()
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
