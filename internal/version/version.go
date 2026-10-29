// Package version carries build metadata set with -ldflags.
package version

var (
	// Version is the release version (set at build time).
	Version = "0.1.0-dev"
	// Commit is the source commit (set at build time).
	Commit = "unknown"
)
