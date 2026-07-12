// Command record-cassette makes ONE real API call and records the exchange
// into a cassette YAML file that internal/cassette.Load parses — for
// building real-data test fixtures for vendors where no open-source project
// publishes recorded traffic (DeepSeek / Zhipu GLM / MiniMax; see
// internal/cassette/recorder's doc comment for the search that came up
// empty, twice).
//
// The API key is read from an environment variable (default RECORD_API_KEY),
// never from a flag, so it stays out of shell history and process listings;
// it is scrubbed from the recording both by header name and by literal
// value (see recorder.RedactValue).
//
// The output path is composed from the recording's coordinates, so every
// self-recorded cassette lands in the same canonical hierarchy under
// testdata/vendor-cassettes/ (resolved via cassette.TestdataPath, so the
// tool works from any working directory):
//
//	<vendor>/<model>/<protocol>/<stream|nostream>/<name>.yaml
//
// vendor/model/name come from flags; protocol defaults to openai (what all
// currently-targeted vendors speak); the stream-vs-nostream bucket is read
// from the request body's own "stream" field rather than asked twice.
//
// Usage — DeepSeek, non-streaming chat:
//
//	echo '{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}' > /tmp/req.json
//	RECORD_API_KEY=sk-... go run ./scripts/record-cassette \
//	  -url https://api.deepseek.com/chat/completions \
//	  -body-file /tmp/req.json \
//	  -vendor deepseek -model deepseek-chat -name chat_basic
//	# -> testdata/vendor-cassettes/deepseek/deepseek-chat/openai/nostream/chat_basic.yaml
//
// Auth styles (-auth): bearer (default) | x-api-key | api-key |
// query:<param> (key-in-URL, e.g. query:key for Gemini AI Studio) | none.
// Repeat -header for protocol-required extras (e.g. -header
// "anthropic-version: 2023-06-01"). -append extends an existing cassette
// written by this tool (turn 2 of a tool-call loop); a multi-turn scenario
// stays in whichever bucket its first turn landed in, even if a later turn
// flips the stream flag (the bucket classifies the scenario, and a file
// can't live in two directories). -out overrides the composed path
// entirely for the odd case that doesn't fit the hierarchy.
//
// After recording: read the file, grep it for anything secret-shaped, and
// document it in testdata/vendor-cassettes/README.md before committing —
// scrubbing removes the credentials this tool knows about, not secrets a
// response body might echo back.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/zereker/llm-gateway/internal/cassette"
	"github.com/zereker/llm-gateway/internal/cassette/recorder"
)

// headerFlags collects repeatable -header "Name: value" flags.
type headerFlags []string

func (h *headerFlags) String() string { return strings.Join(*h, "; ") }
func (h *headerFlags) Set(v string) error {
	if !strings.Contains(v, ":") {
		return fmt.Errorf("want \"Name: value\", got %q", v)
	}
	*h = append(*h, v)
	return nil
}

func main() {
	endpoint := flag.String("url", "", "full endpoint URL (required)")
	bodyFile := flag.String("body-file", "", "request body file, '-' for stdin (required)")
	vendor := flag.String("vendor", "", "vendor directory name, e.g. deepseek / zhipu / minimax")
	model := flag.String("model", "", "model directory name, e.g. deepseek-chat / glm-4-plus")
	protocol := flag.String("protocol", "openai", "wire protocol being recorded (path segment)")
	name := flag.String("name", "", "scenario name (file basename without .yaml)")
	out := flag.String("out", "", "explicit output path (overrides -vendor/-model/-protocol/-name composition)")
	authStyle := flag.String("auth", "bearer", "bearer | x-api-key | api-key | query:<param> | none")
	keyEnv := flag.String("key-env", "RECORD_API_KEY", "environment variable holding the API key")
	appendExisting := flag.Bool("append", false, "prepend the existing cassette (must have been written by this tool)")
	timeout := flag.Duration("timeout", 3*time.Minute, "request timeout (reasoning models can be slow)")
	var headers headerFlags
	flag.Var(&headers, "header", "extra request header \"Name: value\" (repeatable)")
	flag.Parse()

	if *endpoint == "" || *bodyFile == "" {
		log.Fatal("record-cassette: -url and -body-file are required")
	}
	key := os.Getenv(*keyEnv)
	if key == "" && *authStyle != "none" {
		log.Fatalf("record-cassette: environment variable %s is empty (or pass -auth none)", *keyEnv)
	}

	body, err := readBody(*bodyFile)
	if err != nil {
		log.Fatalf("record-cassette: read body: %v", err)
	}

	outPath, err := resolveOutPath(*out, *vendor, *model, *protocol, *name, body, *appendExisting)
	if err != nil {
		log.Fatalf("record-cassette: %v", err)
	}

	rec := recorder.New(nil)
	rec.RedactValue(key)
	if *appendExisting {
		if err := rec.PrependFromFile(outPath); err != nil {
			log.Fatalf("record-cassette: -append: %v", err)
		}
	}

	req, err := buildRequest(*endpoint, body, *authStyle, key, headers, rec)
	if err != nil {
		log.Fatalf("record-cassette: %v", err)
	}

	client := &http.Client{Transport: rec, Timeout: *timeout}
	resp, err := client.Do(req)
	if err != nil {
		log.Fatalf("record-cassette: request failed (nothing recorded): %v", err)
	}
	respBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		log.Fatalf("record-cassette: read response: %v", err)
	}

	fmt.Fprintf(os.Stderr, "HTTP %s\n%s\n", resp.Status, preview(respBody, 2000))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Still record it if asked — a real error response is real data too —
		// but make the operator opt in by reading this and re-running with the
		// body fixed, rather than silently committing an error cassette.
		log.Fatalf("record-cassette: upstream returned %s; not writing %s (fix the request and re-run)", resp.Status, outPath)
	}

	if err := rec.WriteFile(outPath); err != nil {
		log.Fatalf("record-cassette: %v", err)
	}
	fmt.Fprintf(os.Stderr, "\nwrote %d interaction(s) to %s\n", rec.Len(), outPath)
	fmt.Fprintln(os.Stderr, "before committing: read the file, grep it for secrets, and add it to testdata/vendor-cassettes/README.md")
}

func readBody(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

// resolveOutPath composes the canonical self-recorded location
// (<vendor>/<model>/<protocol>/<stream|nostream>/<name>.yaml under
// testdata/vendor-cassettes/) unless -out overrides it. The bucket comes
// from the request body's own "stream" field; on -append, if the composed
// path doesn't exist but the sibling bucket's does, the existing file wins —
// a multi-turn scenario is classified by its first turn, and turn 2 of an
// agent loop is typically non-streaming even when turn 1 streamed.
func resolveOutPath(out, vendor, model, protocol, name string, body []byte, appendExisting bool) (string, error) {
	if out != "" {
		return out, nil
	}
	if vendor == "" || model == "" || name == "" {
		return "", fmt.Errorf("either -out, or all of -vendor/-model/-name, must be set")
	}
	for flagName, v := range map[string]string{"-vendor": vendor, "-model": model, "-protocol": protocol, "-name": name} {
		if strings.ContainsAny(v, `/\`) {
			return "", fmt.Errorf("%s %q must be a single path segment", flagName, v)
		}
	}
	bucket := "nostream"
	if gjson.GetBytes(body, "stream").Bool() {
		bucket = "stream"
	}
	path := cassette.TestdataPath("vendor-cassettes", vendor, model, protocol, bucket, name+".yaml")
	if appendExisting {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			sibling := "stream"
			if bucket == "stream" {
				sibling = "nostream"
			}
			alt := cassette.TestdataPath("vendor-cassettes", vendor, model, protocol, sibling, name+".yaml")
			if _, err := os.Stat(alt); err == nil {
				return alt, nil
			}
		}
	}
	return path, nil
}

func buildRequest(endpoint string, body []byte, authStyle, key string, headers headerFlags, rec *recorder.Recorder) (*http.Request, error) {
	finalURL := endpoint
	if param, ok := strings.CutPrefix(authStyle, "query:"); ok {
		u, err := url.Parse(endpoint)
		if err != nil {
			return nil, fmt.Errorf("parse -url: %w", err)
		}
		q := u.Query()
		q.Set(param, key)
		u.RawQuery = q.Encode()
		finalURL = u.String()
		rec.ScrubQueryParam(param)
	}

	req, err := http.NewRequest("POST", finalURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for _, h := range headers {
		name, value, _ := strings.Cut(h, ":")
		req.Header.Set(strings.TrimSpace(name), strings.TrimSpace(value))
	}
	switch {
	case authStyle == "bearer":
		req.Header.Set("Authorization", "Bearer "+key)
	case authStyle == "x-api-key":
		req.Header.Set("x-api-key", key)
	case authStyle == "api-key":
		req.Header.Set("api-key", key)
	case authStyle == "none", strings.HasPrefix(authStyle, "query:"):
		// handled above / nothing to add
	default:
		return nil, fmt.Errorf("unknown -auth %q (want bearer | x-api-key | api-key | query:<param> | none)", authStyle)
	}
	return req, nil
}

func preview(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + fmt.Sprintf("\n...(truncated, %d bytes total)", len(b))
}
