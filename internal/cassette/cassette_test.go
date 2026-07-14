package cassette

import (
	"path/filepath"
	"testing"
)

func TestTestdataPath_EnvironmentOverride(t *testing.T) {
	t.Setenv("LLM_GATEWAY_TESTDATA_DIR", "/opt/llm-gateway-testdata")

	want := filepath.Join("/opt/llm-gateway-testdata", "fieldmatrix", "endpoints")
	if got := TestdataPath("fieldmatrix", "endpoints"); got != want {
		t.Fatalf("TestdataPath() = %q, want %q", got, want)
	}
}
