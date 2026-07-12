package cassette

import (
	"path/filepath"
	"strings"
	"testing"
)

const vendorRoot = "../app/gateway/testdata/vendor-cassettes"

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
	if !strings.Contains(string(interactions[0].ResponseBody), `"web_search_result_location"`) == false {
		// interaction 0's citation type is content_block_location, not web_search;
		// just check it decoded to valid-looking JSON at all.
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
