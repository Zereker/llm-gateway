// Command mockupstream is a fake upstream for E2E tests: it serves the
// OpenAI Chat Completions / Anthropic Messages / Gemini / Cohere / Bedrock
// upstream routes so the gateway M10 + Flink billing aggregation pipeline can
// be exercised without depending on real vendor keys.
//
// It replays real recorded traffic, nothing else: at startup it loads the
// same internal/cassette/testdata/fieldmatrix/endpoints manifests the gateway seeds from (see
// loadRecordedReplies) and, keyed by upstream model, replays each manifest's
// recorded response — resolved by vendorfixture.ResolveReply from the
// opencassette corpora or a fieldmatrix fixture, the exact data the
// in-process e2e (internal/app/gateway) validates. A model with no manifest
// entry gets a 404 naming the models it does serve; to exercise a new model,
// add a manifest for it rather than teaching this mock to invent data.
//
// Routes (the model is read from the request body, or from the URL for
// Gemini / Bedrock, whose protocols carry it in the path):
//
//	POST /v1/chat/completions  → OpenAI Chat
//	POST /v1/messages          → Anthropic Messages
//	POST /v1beta/models/{model}:generateContent  → Gemini generateContent
//	POST /v2/chat              → Cohere v2/chat
//	POST /openai/deployments/{deployment}/chat/completions  → Azure OpenAI
//	     (same body shape as plain OpenAI Chat; the Azure-specific URL shape
//	     and api-key header are handled on the gateway side)
//	POST /model/{modelId}/converse  → Bedrock Converse
//	GET  /health               → "ok"
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
// body this mock returns for it.
var recordedReplies map[string][]byte

// loadRecordedReplies builds the model -> recorded-body map from the endpoint
// manifests. A manifest that can't be loaded or a reply that can't be resolved
// is logged and skipped (requests for that model then 404) rather than
// aborting startup — the mock must still come up for the health check.
func loadRecordedReplies() map[string][]byte {
	m := map[string][]byte{}

	scenarios, err := vendorfixture.LoadDir(cassette.TestdataPath("fieldmatrix", "endpoints"))
	if err != nil {
		slog.Warn("mockupstream: manifests not loaded; every request will 404", "err", err)
		return m
	}

	for _, sc := range scenarios {
		body, err := vendorfixture.ResolveReply(sc.Reply)
		if err != nil {
			slog.Warn("mockupstream: reply unresolved; model will 404",
				"vendor", sc.Vendor, "model", sc.Model, "err", err)

			continue
		}

		m[sc.Model] = body
	}

	slog.Info("mockupstream: recorded replies loaded", "models", len(m))

	return m
}

// serveRecorded writes the recorded reply for model, or a 404 naming the
// models this mock does serve. The Content-Type mirrors the recorded body's
// shape (SSE vs JSON).
func serveRecorded(w http.ResponseWriter, model string) {
	body, ok := recordedReplies[model]
	if !ok {
		known := make([]string, 0, len(recordedReplies))
		for k := range recordedReplies {
			known = append(known, k)
		}

		http.Error(w, fmt.Sprintf("mockupstream: no recorded reply for model %q (add an internal/cassette/testdata/fieldmatrix/endpoints manifest); known models: %s",
			model, strings.Join(known, ", ")), http.StatusNotFound)

		return
	}

	trimmed := bytes.TrimSpace(body)
	if bytes.HasPrefix(trimmed, []byte("event:")) || bytes.HasPrefix(trimmed, []byte("data:")) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}

	_, _ = w.Write(body)
}

// serveByBodyModel handles the protocols that carry the model in the JSON
// request body (OpenAI / Azure OpenAI / Anthropic / Cohere).
func serveByBodyModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}

	serveRecorded(w, req.Model)
}

// serveGemini extracts the model from a generateContent path
// (/v1beta/models/{model}:generateContent or :streamGenerateContent).
func serveGemini(w http.ResponseWriter, r *http.Request) {
	model := ""

	if i := strings.LastIndex(r.URL.Path, "/"); i >= 0 {
		rest := r.URL.Path[i+1:]
		if j := strings.Index(rest, ":"); j > 0 {
			model = rest[:j]
		}
	}

	serveRecorded(w, model)
}

// serveBedrock extracts the model id from a Converse path
// (/model/{modelId}/converse or /converse-stream).
func serveBedrock(w http.ResponseWriter, r *http.Request) {
	const prefix = "/model/"

	model := ""

	if i := strings.Index(r.URL.Path, prefix); i >= 0 {
		rest := r.URL.Path[i+len(prefix):]
		if j := strings.Index(rest, "/"); j >= 0 {
			rest = rest[:j]
		}

		model = rest
	}

	serveRecorded(w, model)
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
	mux.HandleFunc("/v1/chat/completions", serveByBodyModel)
	mux.HandleFunc("/v1/messages", serveByBodyModel)
	mux.HandleFunc("/v2/chat", serveByBodyModel)
	mux.HandleFunc("/openai/deployments/", serveByBodyModel)
	mux.HandleFunc("/v1beta/models/", serveGemini)
	mux.HandleFunc("/model/", serveBedrock)

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
