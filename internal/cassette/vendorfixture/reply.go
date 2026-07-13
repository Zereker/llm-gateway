package vendorfixture

import (
	"fmt"
	"os"

	"github.com/zereker/opencassette"

	"github.com/zereker/llm-gateway/internal/cassette"
)

// ResolveReply loads the raw upstream response body a scenario's reply points
// at, from the same three sources both e2e consumers understand — so the
// in-process test (internal/app/gateway) and the real-binary mock upstream
// (cmd/mockupstream) resolve a manifest's reply identically, from one place:
//
//   - "fixture":      testdata/fieldmatrix/upstream/<path>, whole file verbatim
//   - "cassette":     response body #Index of a vendor-cassettes VCR file
//   - "opencassette": response body #Index of an opencassette-corpus cassette,
//     read from the corpus embedded in the opencassette module (no submodule
//     or checked-out tree needed)
func ResolveReply(r Reply) ([]byte, error) {
	switch r.Kind {
	case "fixture":
		path := cassette.TestdataPath("fieldmatrix", "upstream", r.Path)
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("vendorfixture: read fixture %s: %w", r.Path, err)
		}
		return b, nil

	case "cassette":
		its, err := cassette.Load(cassette.TestdataPath("vendor-cassettes", r.Path))
		if err != nil {
			return nil, err
		}
		return replyBodyAt(its, r)

	case "opencassette":
		its, err := cassette.LoadFS(opencassette.Corpus(), r.Path)
		if err != nil {
			return nil, err
		}
		return replyBodyAt(its, r)

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
