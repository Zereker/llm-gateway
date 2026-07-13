package cassette

import (
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// TestLoadFS_and_LoadDirFS exercises the fs.FS loaders (the counterparts of
// Load/LoadDir used to read the opencassette module's embedded corpus) against
// a tiny in-memory FS, so they're covered without depending on the on-disk
// corpus or the external module.
func TestLoadFS_and_LoadDirFS(t *testing.T) {
	const doc = `interactions:
- request:
    method: POST
    uri: https://example.test/v1/chat/completions
    body: '{"model":"m","messages":[]}'
  response:
    body:
      string: '{"object":"chat.completion","choices":[{"index":0}]}'
`
	fsys := fstest.MapFS{
		"vendor/model/openai/nostream/basic.yaml": {Data: []byte(doc)},
		"README.md": {Data: []byte("not a cassette — must be ignored")},
	}

	its, err := LoadFS(fsys, "vendor/model/openai/nostream/basic.yaml")
	if err != nil {
		t.Fatalf("LoadFS: %v", err)
	}
	if len(its) != 1 {
		t.Fatalf("LoadFS: want 1 interaction, got %d", len(its))
	}
	if !strings.Contains(string(its[0].RequestBody), `"model":"m"`) {
		t.Errorf("LoadFS: request body not decoded: %q", its[0].RequestBody)
	}
	if !strings.Contains(string(its[0].ResponseBody), "chat.completion") {
		t.Errorf("LoadFS: response body not decoded: %q", its[0].ResponseBody)
	}

	all, err := LoadDirFS(fsys)
	if err != nil {
		t.Fatalf("LoadDirFS: %v", err)
	}
	if len(all) != 1 { // README.md is not *.yaml(.gz) and must be skipped
		t.Fatalf("LoadDirFS: want 1 cassette, got %d: %v", len(all), SortedKeys(all))
	}
	if _, ok := all["vendor/model/openai/nostream/basic.yaml"]; !ok {
		t.Errorf("LoadDirFS: expected key missing, got %v", SortedKeys(all))
	}
}

var vendorRoot = TestdataPath("vendor-cassettes")

func TestLoad_InteractionsFormat(t *testing.T) {
	// simonw's pytest-recording format: top-level `interactions:`.
	path := filepath.Join(vendorRoot, "anthropic/simonw-llm-anthropic/test_tools.yaml")
	interactions, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(interactions) == 0 {
		t.Fatalf("want at least 1 interaction, got 0")
	}
	first := interactions[0]
	if !strings.Contains(string(first.RequestBody), `"model":"claude-haiku`) {
		t.Errorf("request body not decoded: %q", first.RequestBody)
	}
	if len(first.ResponseBody) == 0 {
		t.Fatalf("response body empty")
	}
}

func TestLoad_RequestsResponsesFormat(t *testing.T) {
	// langchain-ai/langchain's own format: parallel `requests:`/`responses:`.
	path := filepath.Join(vendorRoot, "anthropic/langchain-ai-langchain/test_citations.yaml")
	interactions, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(interactions) != 3 {
		t.Fatalf("want 3 interactions, got %d", len(interactions))
	}
	if !strings.Contains(string(interactions[0].RequestBody), `"citations":{"enabled":true}`) {
		t.Errorf("request body not decoded correctly: %q", interactions[0].RequestBody)
	}
	if !strings.HasPrefix(string(interactions[0].ResponseBody), "{") {
		t.Errorf("response body not decoded to JSON: %q", interactions[0].ResponseBody[:min(50, len(interactions[0].ResponseBody))])
	}
	// interaction 1 is the streaming variant (SSE).
	if !strings.HasPrefix(string(interactions[1].ResponseBody), "event:") {
		t.Errorf("streaming response body not decoded to SSE: %q", interactions[1].ResponseBody[:min(50, len(interactions[1].ResponseBody))])
	}
}

func TestLoad_GzippedBinaryBody(t *testing.T) {
	// simonw's cassette with a real gzip-compressed !!binary response body
	// (see test_tools.yaml's second interaction — vision/tool-call responses
	// there are gzip-compressed, unlike test_citations.yaml's plain !!binary).
	path := filepath.Join(vendorRoot, "anthropic/simonw-llm-anthropic/test_tools.yaml")
	interactions, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	found := false
	for _, it := range interactions {
		if strings.Contains(string(it.ResponseBody), `"type":"tool_use"`) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected at least one decompressed response body containing tool_use")
	}
}

func TestLoadDir_CoversAllFiles(t *testing.T) {
	all, err := LoadDir(vendorRoot)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(all) < 100 {
		t.Fatalf("want at least 100 cassette files under vendor-cassettes, found %d", len(all))
	}
	for _, path := range SortedKeys(all) {
		if len(all[path]) == 0 {
			t.Errorf("%s: loaded 0 interactions", path)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
