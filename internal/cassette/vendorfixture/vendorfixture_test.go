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

	// Vendors whose e2e endpoint replies from the opencassette corpus:
	// zhipu/minimax/moonshot are OpenAI-protocol (registered via
	// openai.Aliases()) and had no third-party cassette to borrow at all;
	// gemini/anthropic are the cross-protocol ones, repointed at our own
	// captures so the full-chain e2e exercises real Gemini→OpenAI /
	// Anthropic→OpenAI translation on the corpus. Assert each is wired to the
	// submodule with its expected wire protocol, so a dropped/renamed manifest
	// or a wrong protocol is caught here rather than only inside the
	// MYSQL-gated e2e.
	wantProto := map[string]string{
		"zhipu": "openai", "minimax": "openai", "moonshot": "openai",
		"gemini": "gemini", "anthropic": "anthropic",
	}
	for v, proto := range wantProto {
		sc, ok := byVendor[v]
		if !ok {
			t.Errorf("expected an opencassette-backed manifest for vendor %q", v)
			continue
		}
		if sc.Reply.Kind != "opencassette" {
			t.Errorf("%s: reply.kind = %q, want \"opencassette\"", v, sc.Reply.Kind)
		}
		if sc.Protocol != proto {
			t.Errorf("%s: protocol = %q, want %q", v, sc.Protocol, proto)
		}
	}
}
