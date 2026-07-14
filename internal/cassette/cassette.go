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

// testdataRoot is computed from this package's source location, so callers do
// not depend on their working directory or on the repository's outer layout.
var testdataRoot = func() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "testdata")
}()

// TestdataPath returns an absolute path below internal/cassette/testdata. It is
// safe to call from any package regardless of the test runner's working
// directory. Standalone binaries can override the location with
// LLM_GATEWAY_TESTDATA_DIR.
func TestdataPath(elem ...string) string {
	if root := os.Getenv("LLM_GATEWAY_TESTDATA_DIR"); root != "" {
		return filepath.Join(append([]string{root}, elem...)...)
	}

	return filepath.Join(append([]string{testdataRoot}, elem...)...)
}
