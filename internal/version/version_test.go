package version

import "testing"

func TestString(t *testing.T) {
	oldVersion, oldCommit, oldDate := Version, Commit, BuildDate
	t.Cleanup(func() { Version, Commit, BuildDate = oldVersion, oldCommit, oldDate })

	Version, Commit, BuildDate = "v0.1.0", "abc123", "2026-07-15T00:00:00Z"
	got := String("llm-gateway")
	want := "llm-gateway version=v0.1.0 commit=abc123 date=2026-07-15T00:00:00Z"
	if got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
