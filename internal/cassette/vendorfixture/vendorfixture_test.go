package vendorfixture_test

import (
	"testing"

	"github.com/zereker/llm-gateway/internal/cassette"
	"github.com/zereker/llm-gateway/internal/cassette/vendorfixture"
)

// TestLoadDir_RealManifests loads the actual testdata/fieldmatrix/endpoints
// manifest set and asserts every file parses and validates — a copy-pasted
// manifest with a bad reply.kind or a missing field fails here loudly (naming
// the file) instead of only surfacing inside the MYSQL-gated e2e test, which
// silently skips when no database is configured.
func TestLoadDir_RealManifests(t *testing.T) {
	scenarios, err := vendorfixture.LoadDir(cassette.TestdataPath("fieldmatrix", "endpoints"))
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(scenarios) == 0 {
		t.Fatal("no vendor manifests loaded")
	}

	byVendor := make(map[string]vendorfixture.Scenario, len(scenarios))
	for _, sc := range scenarios {
		if _, dup := byVendor[sc.Vendor]; dup {
			t.Errorf("duplicate vendor manifest: %q", sc.Vendor)
		}
		byVendor[sc.Vendor] = sc
	}

	// The opencassette-backed vendors are the ones that had no third-party
	// cassette to borrow; assert they're wired and pointed at the corpus
	// submodule, so a dropped/renamed manifest is caught here.
	for _, v := range []string{"zhipu", "minimax", "moonshot"} {
		sc, ok := byVendor[v]
		if !ok {
			t.Errorf("expected an opencassette-backed manifest for vendor %q", v)
			continue
		}
		if sc.Reply.Kind != "opencassette" {
			t.Errorf("%s: reply.kind = %q, want \"opencassette\"", v, sc.Reply.Kind)
		}
		if sc.Protocol != "openai" {
			t.Errorf("%s: protocol = %q, want \"openai\"", v, sc.Protocol)
		}
	}
}
