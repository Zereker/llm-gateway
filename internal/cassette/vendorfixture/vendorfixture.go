// Package vendorfixture loads per-vendor endpoint-seed manifests from
// testdata/fieldmatrix/endpoints/ (repo root) — one JSON file per vendor,
// declaring exactly the fields that differ between vendors when seeding a
// business-data endpoint: vendor / protocol / model / auth type / auth
// payload / the upstream path it should be routed to, plus which real
// captured response to reply with.
//
// This is the single source of truth both consumers seed from:
//   - scripts/seed-multivendor: seeds real MySQL rows for a real cmd/gateway
//   - cmd/mockupstream black-box run.
//   - internal/app/gateway's TestE2E_MultiVendor_AllProtocols: seeds the same
//     shape in-process against an httptest mock.
//
// Adding a new vendor to *both* is one new JSON file here — see any existing
// file for the shape, and cmd/mockupstream's doc comment for which
// UpstreamPath values it actually serves.
package vendorfixture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Reply picks which real captured data a mock upstream should hand back for
// this scenario. Kind "cassette" reads response body #Index from a real VCR
// file under testdata/vendor-cassettes/Path; kind "opencassette" reads
// response body #Index from a cassette in the opencassette corpus submodule
// (testdata/opencassette/corpus/Path) — our own purpose-recorded captures,
// notably for vendors (Zhipu / MiniMax / Moonshot) that have no third-party
// cassette to borrow; kind "fixture" reads the whole file verbatim from
// testdata/fieldmatrix/upstream/Path (a curated/sanitized derivative, for
// vendors where the raw cassette shape needs adapting first).
type Reply struct {
	Kind  string `json:"kind"`
	Path  string `json:"path"`
	Index int    `json:"index"`
}

// Scenario is one vendor's endpoint-seed manifest.
type Scenario struct {
	Vendor       string          `json:"vendor"`
	Protocol     string          `json:"protocol"`
	Model        string          `json:"model"`
	AuthType     string          `json:"auth_type"`
	ClientAPIKey string          `json:"client_api_key"` // plaintext key the (mock) client authenticates with
	UpstreamAuth json.RawMessage `json:"upstream_auth"`  // encrypted into endpoints.auth as AuthType's payload
	UpstreamPath string          `json:"upstream_path"`  // path on the mockupstream instance this endpoint should route to
	Reply        Reply           `json:"reply"`
}

// LoadDir reads every *.json file in dir (sorted by filename, for
// deterministic seed order) and unmarshals each into a Scenario, failing
// loudly — naming the file — on a missing required field, so a copy-pasted
// manifest with a forgotten field doesn't silently seed a broken endpoint.
func LoadDir(dir string) ([]Scenario, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("vendorfixture: read %s: %w", dir, err)
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			names = append(names, e.Name())
		}
	}

	sort.Strings(names)

	scenarios := make([]Scenario, 0, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)

		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("vendorfixture: read %s: %w", path, err)
		}

		var sc Scenario
		if err := json.Unmarshal(b, &sc); err != nil {
			return nil, fmt.Errorf("vendorfixture: unmarshal %s: %w", path, err)
		}

		if err := sc.validate(); err != nil {
			return nil, fmt.Errorf("vendorfixture: %s: %w", path, err)
		}

		scenarios = append(scenarios, sc)
	}

	return scenarios, nil
}

func (sc Scenario) validate() error {
	for field, val := range map[string]string{
		"vendor": sc.Vendor, "protocol": sc.Protocol, "model": sc.Model,
		"auth_type": sc.AuthType, "client_api_key": sc.ClientAPIKey, "upstream_path": sc.UpstreamPath,
	} {
		if val == "" {
			return fmt.Errorf("missing required field %q", field)
		}
	}

	if len(sc.UpstreamAuth) == 0 {
		return fmt.Errorf("missing required field %q", "upstream_auth")
	}

	switch sc.Reply.Kind {
	case "cassette", "opencassette", "fixture":
	default:
		return fmt.Errorf("reply.kind must be one of %q/%q/%q, got %q", "cassette", "opencassette", "fixture", sc.Reply.Kind)
	}

	if sc.Reply.Path == "" {
		return fmt.Errorf("missing required field %q", "reply.path")
	}

	return nil
}
