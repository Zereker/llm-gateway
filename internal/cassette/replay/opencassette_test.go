package replay

// This file replays the opencassette corpus — the purpose-built companion
// dataset vendored as a git submodule at testdata/opencassette (module
// github.com/zereker/opencassette). Unlike vendor-cassettes/ (third-party VCR
// fixtures incidentally published by langchain / simonw), the opencassette
// corpus is recorded by us against our own scenario packs, and crucially
// covers vendors for which no public recorded traffic existed at all — Zhipu
// GLM, MiniMax, Moonshot Kimi — plus fresh AWS Bedrock (Anthropic wire),
// Azure (OpenAI + Responses) and Google Gemini captures.
//
// The corpus layout is <vendor>/<model>/<protocol>/<stream|nostream>/<scenario>.yaml,
// so the wire protocol of every file is its third path segment. This test
// routes each file by that segment through the matching real gateway code —
// the openai_gemini / openai_anthropic translators for the cross-protocol
// vendors, the usage extractor for the OpenAI-native ones — and, like
// TestZZZ_Completeness does for vendor-cassettes/, fails loudly if any file is
// neither exercised nor consciously accounted for, so a newly-recorded vendor
// can never silently land without a replay test covering it.

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/zereker/opencassette"

	"github.com/zereker/llm-gateway/internal/cassette"
	"github.com/zereker/llm-gateway/internal/translator/openai_anthropic"
	"github.com/zereker/llm-gateway/internal/translator/openai_gemini"
	"github.com/zereker/llm-gateway/internal/usage/extractor"
)

// protocolOf returns the wire-protocol segment of a corpus-relative path
// (<vendor>/<model>/<protocol>/<stream|nostream>/<scenario>.yaml). Returns ""
// for a path that doesn't have that shape (e.g. the corpus README).
func protocolOf(rel string) string {
	parts := strings.Split(rel, "/")
	if len(parts) < 5 {
		return ""
	}
	return parts[2]
}

// hasUsage reports whether a raw upstream response body carries an actual
// usage *object* — an OpenAI/Anthropic `"usage":{...}` or a Gemini
// `usageMetadata`. It deliberately matches the object form, not the bare word:
// MiniMax's forced-tool streaming chunks each carry `"usage":null` (the field
// is present but empty for that scenario), and a substring match on "usage"
// there would wrongly demand the extractor produce usage the vendor never
// sent. Used to gate the usage assertion so we only require extraction where
// the upstream genuinely reported usage — not to flag a gateway bug where
// there's only a vendor quirk.
func hasUsage(body []byte) bool {
	return bytes.Contains(body, []byte(`"usage":{`)) ||
		bytes.Contains(body, []byte(`"usage": {`)) ||
		bytes.Contains(body, []byte("usageMetadata"))
}

// isSSE reports whether body is a Server-Sent-Events stream (as opposed to a
// single JSON document) — recognized by a leading event:/data: line.
func isSSE(body []byte) bool {
	s := strings.TrimSpace(string(body))
	return strings.HasPrefix(s, "data:") || strings.HasPrefix(s, "event:")
}

// looksLikeOpenAIChatBody reports whether body is a Chat Completions response
// (streaming or not). Tolerant of whitespace — real vendors pretty-print
// (`"object": "chat.completion"`) as often as they minify — so it can't reuse
// classifyOpenAIResponse, which pins the minified form the langchain corpus
// happened to use.
func looksLikeOpenAIChatBody(body []byte) bool {
	if isSSE(body) {
		return bytes.Contains(body, []byte("chat.completion.chunk"))
	}
	return bytes.Contains(body, []byte("chat.completion"))
}

// looksLikeResponsesBody reports whether body is a Responses API response
// (streaming or not), tolerant of whitespace the same way.
func looksLikeResponsesBody(body []byte) bool {
	if isSSE(body) {
		return bytes.Contains(body, []byte("response."))
	}
	return bytes.Contains(body, []byte(`"object"`)) && bytes.Contains(body, []byte("response"))
}

// hasDoneTerminator reports whether an SSE stream ends with the OpenAI
// `data: [DONE]` sentinel. Not every real OpenAI-compatible upstream sends it
// (MiniMax, confirmed in this corpus, does not) — this is used only to
// *record* that fact in the test log, never to fail, because for an
// OpenAI-native upstream the identity translator passes the stream through
// verbatim and inventing a [DONE] the upstream never sent isn't this replay
// test's job to assert.
func hasDoneTerminator(body []byte) bool {
	return bytes.Contains(body, []byte("[DONE]"))
}

// assertWellFormedOpenAIChatBody checks a raw OpenAI-native upstream body is
// something the client can consume unchanged: a JSON chat.completion with a
// non-empty choices array, or an SSE stream whose data lines are each valid
// JSON. Deliberately weaker than assertValidOpenAIChatOutput (no [DONE]
// requirement) — see the "openai" case comment.
func assertWellFormedOpenAIChatBody(t *testing.T, body []byte, label string) {
	t.Helper()
	if !isSSE(body) {
		if !json.Valid(body) {
			t.Fatalf("%s: non-streaming body is not valid JSON: %s", label, truncate(body, 300))
		}
		var probe struct {
			Choices []json.RawMessage `json:"choices"`
		}
		if err := json.Unmarshal(body, &probe); err != nil {
			t.Fatalf("%s: body has no parseable choices field: %v", label, err)
		}
		if len(probe.Choices) == 0 {
			t.Fatalf("%s: body has 0 choices: %s", label, truncate(body, 300))
		}
		return
	}
	sawData := false
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			continue
		}
		sawData = true
		if !json.Valid([]byte(payload)) {
			t.Fatalf("%s: SSE data line is not valid JSON: %s", label, payload)
		}
	}
	if !sawData {
		t.Fatalf("%s: SSE stream had no data payloads", label)
	}
	if !hasDoneTerminator(body) {
		t.Logf("%s: NOTE — real stream has no `data: [DONE]` terminator (vendor quirk; passed through verbatim)", label)
	}
}

// TestReplayOpenCassetteCorpus walks every cassette in the opencassette
// corpus and, routing by wire protocol, feeds each real upstream response
// through the exact gateway path a client request to that vendor would take:
//
//   - anthropic  (AWS Bedrock Claude): openai_anthropic response handler,
//     asserting the Anthropic Messages response translates to well-formed
//     OpenAI output the client SDK could parse.
//   - gemini     (Google): openai_gemini response handler, same assertion.
//   - openai     (Zhipu / MiniMax / Moonshot / Azure OpenAI): OpenAI-native,
//     so the gateway path is pass-through + usage extraction — asserted via
//     the OpenAI extractor and a well-formed-OpenAI-shape check on the raw body.
//   - openai-responses (Azure Responses): Responses-native, likewise
//     pass-through + the Responses extractor.
//
// Every file must be accounted for: examined (>=1 classified interaction) or,
// if some future file legitimately can't be, it must be added to the
// switch with a stated reason. A recognized-protocol file with zero usable
// interactions is a hard failure (a truncated/corrupt capture), not a skip.
func TestReplayOpenCassetteCorpus(t *testing.T) {
	// The corpus is embedded in the opencassette module (opencassette.Corpus()
	// returns an fs.FS rooted at corpus/), so unlike a submodule it's always
	// present once the dependency resolves — a zero-file result would mean a
	// broken/empty embed, which is a hard failure, not a skip.
	files, err := cassette.LoadDirFS(opencassette.Corpus())
	if err != nil {
		t.Fatalf("LoadDirFS(opencassette.Corpus()): %v", err)
	}
	if len(files) == 0 {
		t.Fatal("opencassette.Corpus() embedded zero cassettes — the dependency's corpus embed is empty")
	}

	anthropicTr := openai_anthropic.New()
	geminiTr := openai_gemini.New()

	var unaccounted []string
	examinedFiles := 0

	for _, rel := range cassette.SortedKeys(files) {
		proto := protocolOf(rel)
		interactions := files[rel]
		examined := false

		for i, it := range interactions {
			body := it.ResponseBody
			if len(body) == 0 {
				continue
			}

			label := rel + "#" + strconv.Itoa(i)

			switch proto {
			case "anthropic":
				if !looksLikeAnthropicMessageResponse(body) {
					continue
				}
				examined = true
				t.Run(label, func(t *testing.T) {
					h := anthropicTr.NewResponseHandler()
					out, usage := feedResponse(t, h, body, label)
					assertValidOpenAIChatOutput(t, out, label)
					if hasUsage(body) && usage == nil {
						t.Errorf("%s: Anthropic response reported usage but extractor returned nil", label)
					}
				})

			case "gemini":
				if !looksLikeGeminiResponse(body) {
					continue
				}
				examined = true
				t.Run(label, func(t *testing.T) {
					h := geminiTr.NewResponseHandler()
					out, usage := feedResponse(t, h, body, label)
					assertValidOpenAIChatOutput(t, out, label)
					if hasUsage(body) && usage == nil {
						t.Errorf("%s: Gemini response reported usage but extractor returned nil", label)
					}
				})

			case "openai":
				if !looksLikeOpenAIChatBody(body) {
					continue
				}
				examined = true
				t.Run(label, func(t *testing.T) {
					// OpenAI-native upstream: the gateway path is pass-through
					// (identity translator) + usage extraction, so we assert the
					// raw upstream body — the bytes the client receives unchanged
					// — is well-formed, plus that usage extracts. We deliberately
					// do NOT require a `data: [DONE]` terminator here (unlike
					// assertValidOpenAIChatOutput, which pins *our* output
					// contract): MiniMax's real streams end on a final usage
					// chunk with no [DONE] sentinel, and the identity handler
					// passes that through verbatim — see hasDoneTerminator below.
					assertWellFormedOpenAIChatBody(t, body, label)
					s := extractor.NewOpenAI()
					s.Feed(body)
					if hasUsage(body) && s.Final() == nil {
						t.Errorf("%s: OpenAI response reported usage but extractor returned nil", label)
					}
				})

			case "openai-responses":
				if !looksLikeResponsesBody(body) {
					continue
				}
				examined = true
				t.Run(label, func(t *testing.T) {
					assertValidResponsesOutput(t, body, label)
					s := extractor.NewResponses()
					s.Feed(body)
					if hasUsage(body) && s.Final() == nil {
						t.Errorf("%s: Responses response reported usage but extractor returned nil", label)
					}
				})

			default:
				// An unrecognized protocol segment means a new wire protocol
				// was recorded into the corpus without wiring a replay branch
				// here — surface it as unaccounted below.
			}
		}

		if examined {
			examinedFiles++
		} else {
			unaccounted = append(unaccounted, rel)
		}
	}

	if len(unaccounted) > 0 {
		t.Errorf("%d opencassette corpus file(s) had no classifiable response for their wire protocol (new protocol needing a replay branch, or a truncated/corrupt capture):", len(unaccounted))
		for _, u := range unaccounted {
			t.Errorf("  %s (protocol=%q)", u, protocolOf(u))
		}
	}
	t.Logf("opencassette corpus coverage: %d files, all exercised through the matching gateway path", examinedFiles)
}

// assertValidResponsesOutput checks that out is a well-formed OpenAI Responses
// payload — either a single response JSON object (`"object":"response"`), or
// an SSE stream of typed `response.*` events whose data lines are valid JSON.
// The Responses-API counterpart to assertValidOpenAIChatOutput (which asserts
// the Chat Completions `choices` shape and would reject a Responses body).
func assertValidResponsesOutput(t *testing.T, out []byte, label string) {
	t.Helper()
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		t.Fatalf("%s: empty output", label)
	}
	if !strings.HasPrefix(trimmed, "data:") && !strings.HasPrefix(trimmed, "event:") {
		if !json.Valid(out) {
			t.Fatalf("%s: non-streaming Responses output is not valid JSON: %s", label, truncate(out, 300))
		}
		if !strings.Contains(trimmed, `"object":"response"`) && !strings.Contains(trimmed, `"object": "response"`) {
			t.Fatalf("%s: non-streaming output isn't a Responses object: %s", label, truncate(out, 300))
		}
		return
	}
	sawResponseEvent := false
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "event:") {
			if strings.Contains(line, "response.") {
				sawResponseEvent = true
			}
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			continue
		}
		if !json.Valid([]byte(payload)) {
			t.Fatalf("%s: Responses SSE data line is not valid JSON: %s", label, payload)
		}
		if strings.Contains(payload, `"response.`) || strings.Contains(payload, `"type":"response`) {
			sawResponseEvent = true
		}
	}
	if !sawResponseEvent {
		t.Fatalf("%s: SSE stream contained no response.* event", label)
	}
}
