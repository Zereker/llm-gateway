// Package cassette loads real VCR cassette fixtures (recorded HTTP
// request/response pairs) vendored under
// internal/app/gateway/testdata/vendor-cassettes/ so translator/extractor
// tests can replay real upstream traffic instead of hand-written literals.
//
// Two on-disk cassette formats are supported (both used across the vendored
// sources — see that directory's README for provenance):
//
//   - pytest-recording's own format: a top-level `interactions:` list, each
//     with a `request`/`response` pair.
//   - langchain-ai/langchain's own format: parallel top-level `requests:`/
//     `responses:` lists, index-aligned.
//
// A body may be a plain YAML string, a `!!binary` scalar (base64,
// gzip-compressed or not), or — in pytest-recording's variant — nested one
// level under a `string:` key. Load normalizes all of that into plain []byte,
// transparently gunzipping where the gzip magic bytes are present.
package cassette

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// Interaction is one normalized request/response pair from a cassette.
type Interaction struct {
	Method       string
	URI          string
	RequestBody  []byte // nil for a bodyless request (e.g. GET)
	ResponseBody []byte // nil if the response genuinely had no body
}

// Load reads a single cassette YAML file and returns its interactions in
// recorded order.
func Load(path string) ([]Interaction, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cassette: read %s: %w", path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("cassette: parse %s: %w", path, err)
	}

	if interactions, ok := doc["interactions"].([]any); ok {
		out := make([]Interaction, 0, len(interactions))
		for _, item := range interactions {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			out = append(out, buildInteraction(m["request"], m["response"]))
		}
		return out, nil
	}

	reqs, _ := doc["requests"].([]any)
	resps, _ := doc["responses"].([]any)
	n := len(reqs)
	if len(resps) > n {
		n = len(resps)
	}
	out := make([]Interaction, 0, n)
	for i := 0; i < n; i++ {
		var req, resp any
		if i < len(reqs) {
			req = reqs[i]
		}
		if i < len(resps) {
			resp = resps[i]
		}
		out = append(out, buildInteraction(req, resp))
	}
	return out, nil
}

func buildInteraction(req, resp any) Interaction {
	var it Interaction
	if m, ok := req.(map[string]any); ok {
		it.Method, _ = m["method"].(string)
		it.URI, _ = m["uri"].(string)
		it.RequestBody = decodeBody(m["body"])
	}
	if m, ok := resp.(map[string]any); ok {
		it.ResponseBody = decodeBody(m["body"])
	}
	return it
}

// decodeBody normalizes a YAML-decoded body value (string / !!binary-decoded
// string / {"string": ...} / nil) into raw bytes, gunzipping transparently
// when the gzip magic number is present — some recorders store the body
// gzip-compressed under the !!binary tag, others store the same tag for
// bytes that just happen to need base64 (not actually gzip); relying on the
// magic number instead of the file format handles both.
func decodeBody(v any) []byte {
	switch b := v.(type) {
	case nil:
		return nil
	case string:
		return gunzipIfNeeded([]byte(b))
	case map[string]any:
		return decodeBody(b["string"])
	default:
		return nil
	}
}

func gunzipIfNeeded(b []byte) []byte {
	if len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		return b
	}
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return b
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		return b
	}
	return out
}

// LoadDir walks dir recursively and loads every *.yaml file found. The
// returned map is keyed by path relative to dir (forward-slash separated),
// sorted access via Paths for deterministic iteration.
func LoadDir(dir string) (map[string][]Interaction, error) {
	out := make(map[string][]Interaction)
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".yaml" {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		interactions, err := Load(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = interactions
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SortedKeys returns m's keys sorted, for deterministic test iteration order.
func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
