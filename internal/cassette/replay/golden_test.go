package replay

import (
	"bytes"
	"os"
	"regexp"
	"testing"

	"github.com/zereker/llm-gateway/internal/cassette"
)

// updateGolden regenerates fixtures under testdata/fieldmatrix/golden/
// instead of comparing against them -- run as
//
//	UPDATE_GOLDEN=1 go test ./internal/cassette/replay/... -run TestGolden
//
// then read the diff and manually verify the new output is actually correct
// before committing it. This write path trusts whatever the translator
// currently produces; it is not itself a correctness check.
var updateGolden = os.Getenv("UPDATE_GOLDEN") == "1"

var (
	reGoldenID      = regexp.MustCompile(`"id":"chatcmpl-[0-9a-f]+"`)
	reGoldenCreated = regexp.MustCompile(`"created":\d+`)
)

// normalizeGolden replaces the two fields every openai_* translator
// generates itself (never sourced from the real upstream cassette) -- a
// fresh random id per response, and a current-time "created" timestamp --
// with fixed placeholders, so a golden comparison isn't flaky by
// construction.
func normalizeGolden(b []byte) []byte {
	b = reGoldenID.ReplaceAll(b, []byte(`"id":"chatcmpl-GOLDEN"`))
	b = reGoldenCreated.ReplaceAll(b, []byte(`"created":0`))
	return b
}

// assertGolden is a stricter companion to assertValidOpenAIChatOutput: the
// latter only checks "is this a well-formed OpenAI response" (so a
// translator bug that swaps two fields but keeps the shape valid slips
// through); this checks the exact bytes match a fixture that was reviewed
// by hand once. Use it sparingly, on a handful of scenarios worth pinning
// down precisely -- not as a replacement for the shape-only check across
// the whole real-cassette corpus, which stays useful for "did this crash or
// produce garbage" on everything else.
func assertGolden(t *testing.T, name string, actual []byte) {
	t.Helper()
	actual = normalizeGolden(actual)
	path := cassette.TestdataPath("fieldmatrix", "golden", name)
	if updateGolden {
		if err := os.WriteFile(path, actual, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden %s missing (run UPDATE_GOLDEN=1 and manually verify the output before committing it): %v", path, err)
	}
	if !bytes.Equal(want, actual) {
		t.Errorf("output does not match golden %s\n--- want ---\n%s\n--- got ---\n%s", path, want, actual)
	}
}
