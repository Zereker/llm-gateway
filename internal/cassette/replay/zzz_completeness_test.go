package replay

import (
	"testing"

	"github.com/zereker/llm-gateway/internal/cassette"
)

// TestZZZ_Completeness must run after every other test in this package: Go
// runs a package's top-level Test functions file-by-file in filename sort
// order (then declaration order within each file), so this lives in
// zzz_completeness_test.go specifically to sort after anthropic/cohere/
// gemini/openai/replay_test.go — putting the check in, say,
// completeness_test.go would silently run it third (after "cohere", before
// "gemini"), long before most claim() calls have happened, and every file
// examined by a not-yet-run test would look unaccounted for. The ZZZ prefix
// on the function name is a secondary guard against a future file that
// would otherwise sort even later than "zzz_".
//
// It is the actual enforcement of "not a single case gets silently dropped":
// every *.yaml file cassette.LoadDir finds under vendor-cassettes must have
// been either claim()ed by a replay subtest above, or explicitly
// markNotApplicable() with a reason. A file matching neither means some
// classifier regressed (or a new vendor cassette source was vendored without
// wiring a replay test for it) — that is a hard failure, not a warning.
func TestZZZ_Completeness(t *testing.T) {
	all, err := cassette.LoadDir(vendorRoot)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	var unaccounted []string
	for path := range all {
		if claimed[path] || notApplicable[path] != "" {
			continue
		}
		unaccounted = append(unaccounted, path)
	}
	if len(unaccounted) > 0 {
		t.Errorf("%d cassette file(s) neither claimed nor marked not-applicable by any replay test:", len(unaccounted))
		for _, p := range cassette.SortedKeys(all) {
			for _, u := range unaccounted {
				if u == p {
					t.Errorf("  %s", p)
				}
			}
		}
	}
	t.Logf("cassette coverage: %d files total, %d claimed by a replay test, %d explicitly out of scope",
		len(all), len(claimed), len(notApplicable))
}
