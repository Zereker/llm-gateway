// Package version exposes build metadata for the shipped commands.
package version

import "fmt"

// These values are replaced with -ldflags for tagged builds. Development
// builds keep explicit, recognizable values instead of pretending to be a
// release.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// String returns one stable, machine-readable line shared by every command.
func String(command string) string {
	return fmt.Sprintf("%s version=%s commit=%s date=%s", command, Version, Commit, BuildDate)
}
