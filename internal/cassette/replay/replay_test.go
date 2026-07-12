// Package replay replays every real cassette under
// testdata/vendor-cassettes/ (repo root) through the actual
// translator / usage-extractor implementations, so the whole vendored real
// upstream traffic corpus is exercised — not just the hand-picked highlight
// cases inline in each translator's own tests.
//
// Every subtest records which cassette-relative-paths it examined into a
// shared, mutex-protected set; TestZZZ_Completeness (named to run last — see
// the note above that function) diffs that against cassette.LoadDir's full
// file listing and fails loudly, naming the file, if anything was never
// touched. That is what "not a single case gets silently dropped" means in
// practice: a file that stops being claimed (a vendor adds a new cassette, or
// a bug in the classifiers below skips it) turns into a hard test failure
// instead of quietly vanishing from coverage.
package replay

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/zereker/llm-gateway/internal/cassette"
	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/translator"
)

var vendorRoot = cassette.TestdataPath("vendor-cassettes")

// claimed / notApplicable together must account for every file cassette.LoadDir
// finds under vendorRoot. claimed = "a replay subtest fed at least one
// interaction from this file through real gateway code". notApplicable =
// "inspected and consciously out of scope" (e.g. an Embeddings-API cassette —
// there is no chat/response translator for that), with a reason string so it
// reads as a decision, not an oversight.
var (
	mu            sync.Mutex
	claimed       = map[string]bool{}
	notApplicable = map[string]string{}
)

func claim(path string) {
	mu.Lock()
	defer mu.Unlock()
	claimed[path] = true
}

func markNotApplicable(path, reason string) {
	mu.Lock()
	defer mu.Unlock()
	notApplicable[path] = reason
}

// feedResponse runs body through h.Feed then h.Flush — the same sequence M7
// runs for a buffer-then-translate response handler — and returns the
// concatenated client bytes plus extracted usage.
func feedResponse(t *testing.T, h translator.ResponseHandler, body []byte, label string) ([]byte, *domain.Usage) {
	t.Helper()
	out1, err := h.Feed(body)
	if err != nil {
		t.Fatalf("%s: Feed error: %v", label, err)
	}
	out2, usage, err := h.Flush()
	if err != nil {
		t.Fatalf("%s: Flush error: %v", label, err)
	}
	return append(out1, out2...), usage
}

// assertValidOpenAIChatOutput checks that out is either a well-formed OpenAI
// chat.completion JSON body, or a well-formed SSE stream of
// chat.completion.chunk events terminated by "data: [DONE]" — the two shapes
// every openai_* translator's response handler produces.
func assertValidOpenAIChatOutput(t *testing.T, out []byte, label string) {
	t.Helper()
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		t.Fatalf("%s: empty output", label)
	}
	if !strings.HasPrefix(trimmed, "data:") && !strings.HasPrefix(trimmed, "event:") {
		if !json.Valid(out) {
			t.Fatalf("%s: non-streaming output is not valid JSON: %s", label, truncate(out, 300))
		}
		var probe struct {
			Choices []json.RawMessage `json:"choices"`
		}
		if err := json.Unmarshal(out, &probe); err != nil {
			t.Fatalf("%s: output has no parseable choices field: %v", label, err)
		}
		if len(probe.Choices) == 0 {
			t.Fatalf("%s: output has 0 choices: %s", label, truncate(out, 300))
		}
		return
	}
	sawDone := false
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			sawDone = true
			continue
		}
		if !json.Valid([]byte(payload)) {
			t.Fatalf("%s: SSE data line is not valid JSON: %s", label, payload)
		}
	}
	if !sawDone {
		t.Fatalf("%s: SSE stream never sent data: [DONE]", label)
	}
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}

// vendorReplayConfig is the shared skeleton every "upstream-only" vendor's
// TestReplayXResponses follows (see gemini_test.go / cohere_test.go for the
// two current instances). Adding a genuinely new vendor to this suite is:
// one dirs list, one classify function, and a call to runResponseReplay —
// nothing else in this package needs to change.
type vendorReplayConfig struct {
	dirs []string
	// classify reports whether a response body is this vendor's real chat
	// response shape (as opposed to a request-only interaction, or some
	// other payload the corpus happens to contain).
	classify func(body []byte) bool
	// translator is the openai_<vendor> translator whose response handler
	// gets fed every classified body.
	translator translator.Translator
	// notApplicableReason is called once per file with zero classified
	// interactions, and must return a non-empty reason — an empty return is
	// a bug (see markNotApplicable's doc comment: every out-of-scope file
	// needs a stated reason, not a silent skip). Most vendors return the
	// same string regardless of relPath; Cohere's is the one example that
	// varies by file (see cohere_test.go).
	notApplicableReason func(relPath string) string
	// skipUsageCheck, if non-nil, reports whether this response body is
	// expected to yield no usage (e.g. Anthropic error responses) — usage
	// absence is only flagged as a bug when this is nil or returns false.
	skipUsageCheck func(body []byte) bool
}

// runResponseReplay feeds every interaction under cfg.dirs whose response
// body cfg.classify accepts through cfg.translator's response handler,
// asserting it doesn't error and produces well-formed OpenAI-shaped output —
// then accounts for every file via claim/markNotApplicable so
// TestZZZ_Completeness can enforce that nothing was silently skipped.
func runResponseReplay(t *testing.T, cfg vendorReplayConfig) {
	t.Helper()
	for _, dir := range cfg.dirs {
		files, err := cassette.LoadDir(vendorRoot + "/" + dir)
		if err != nil {
			t.Fatalf("LoadDir %s: %v", dir, err)
		}
		for _, rel := range cassette.SortedKeys(files) {
			path := dir + "/" + rel
			interactions := files[rel]
			examined := false
			for i, it := range interactions {
				if len(it.ResponseBody) == 0 || !cfg.classify(it.ResponseBody) {
					continue
				}
				examined = true
				i, it := i, it
				t.Run(path+"#"+strconv.Itoa(i), func(t *testing.T) {
					h := cfg.translator.NewResponseHandler()
					out, usage := feedResponse(t, h, it.ResponseBody, path)
					assertValidOpenAIChatOutput(t, out, path)
					skip := cfg.skipUsageCheck != nil && cfg.skipUsageCheck(it.ResponseBody)
					if !skip && usage == nil {
						t.Errorf("%s: expected non-nil usage", path)
					}
				})
			}
			if examined {
				claim(path)
			} else {
				reason := cfg.notApplicableReason(rel)
				if reason == "" {
					t.Fatalf("%s: notApplicableReason returned an empty string — every out-of-scope file needs a stated reason", path)
				}
				markNotApplicable(path, reason)
			}
		}
	}
}

// runRequestWellFormedCheck is the shared skeleton for a vendor with no
// reverse-direction translator (upstream-only, like Gemini/Cohere): there's
// nothing to translate on the request side, but this still guards against a
// corrupted/truncated cassette silently masquerading as "out of scope".
func runRequestWellFormedCheck(t *testing.T, dirs []string) {
	t.Helper()
	for _, dir := range dirs {
		files, err := cassette.LoadDir(vendorRoot + "/" + dir)
		if err != nil {
			t.Fatalf("LoadDir %s: %v", dir, err)
		}
		for _, rel := range cassette.SortedKeys(files) {
			path := dir + "/" + rel
			for i, it := range files[rel] {
				if len(it.RequestBody) == 0 {
					continue
				}
				if !json.Valid(it.RequestBody) {
					t.Errorf("%s#%d: request body is not valid JSON", path, i)
				}
			}
		}
	}
}
