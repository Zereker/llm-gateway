package vendorfixture

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/zereker/opencassette"
	"github.com/zereker/opencassette/cassette"

	fixturepath "github.com/zereker/llm-gateway/internal/cassette"
)

// ResolveReply loads the raw upstream response body a scenario's reply points
// at, from the same two sources both e2e consumers understand — so the
// in-process test (internal/app/gateway) and the real-binary mock upstream
// (examples/support/mockupstream) resolve a manifest's reply identically, from one place:
//
//   - "cassette": response body #Index of a real recorded cassette from the
//     opencassette module — looked up in Corpus() (our own recordings) first,
//     then Vendored() (the third-party corpus). The two trees' top-level
//     vendor directories are disjoint, so a path is never ambiguous.
//   - "fixture":  internal/cassette/testdata/fieldmatrix/upstream/<path>, whole file verbatim
//     (a curated/sanitized derivative).
func ResolveReply(r Reply) ([]byte, error) {
	switch r.Kind {
	case "cassette":
		its, err := cassette.LoadFS(opencassette.Corpus(), r.Path)
		if errors.Is(err, fs.ErrNotExist) {
			its, err = cassette.LoadFS(opencassette.Vendored(), r.Path)
		}

		if err != nil {
			return nil, fmt.Errorf("vendorfixture: %s not found in either opencassette corpus (own or vendored): %w", r.Path, err)
		}

		return replyBodyAt(its, r)

	case "fixture":
		path := fixturepath.TestdataPath("fieldmatrix", "upstream", r.Path)

		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("vendorfixture: read fixture %s: %w", r.Path, err)
		}

		return b, nil

	default:
		return nil, fmt.Errorf("vendorfixture: unknown reply.kind %q", r.Kind)
	}
}

func replyBodyAt(its []cassette.Interaction, r Reply) ([]byte, error) {
	if r.Index >= len(its) {
		return nil, fmt.Errorf("vendorfixture: %s: want interaction #%d, only %d recorded", r.Path, r.Index, len(its))
	}

	body := its[r.Index].ResponseBody
	if len(body) == 0 {
		return nil, fmt.Errorf("vendorfixture: %s#%d: empty response body", r.Path, r.Index)
	}

	return body, nil
}
