// Package cassette holds what's left of the gateway's cassette plumbing: a
// single path helper for the repo's own on-disk fixtures. The actual cassette
// loaders (LoadFS / LoadDirFS, both wire formats, transparent gunzip) live in
// the opencassette module — github.com/zereker/opencassette/cassette — next to
// the embedded corpora they read (opencassette.Corpus() / Vendored()); import
// that package directly instead of duplicating a parser here.
package cassette

import (
	"os"
	"path/filepath"
	"runtime"
)

// repoRoot is computed once from this source file's own location —
// internal/cassette/cassette.go is exactly two directories below the repo
// root, so this is stable no matter which package imports it or what
// directory `go test` happened to set as the working directory.
var repoRoot = func() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile))) // .../internal/cassette/cassette.go -> internal/cassette -> internal -> repo root
}()

// TestdataPath returns an absolute path to testdata/<elem...> at the repo
// root (e.g. TestdataPath("fieldmatrix", "endpoints") ->
// "<repo>/testdata/fieldmatrix/endpoints"). Safe to call from any package's
// test file regardless of nesting depth — unlike a hand-counted relative path
// ("../../testdata/..."), it doesn't silently break if either the caller or
// testdata/ itself moves one level.
func TestdataPath(elem ...string) string {
	if root := os.Getenv("LLM_GATEWAY_TESTDATA_DIR"); root != "" {
		return filepath.Join(append([]string{root}, elem...)...)
	}

	return filepath.Join(append([]string{repoRoot, "testdata"}, elem...)...)
}
