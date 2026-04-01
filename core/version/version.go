// Package version provides build-time version information for Aileron binaries.
package version

// Set at build time via ldflags:
//
//	-X github.com/ALRubinger/aileron/core/version.Version=0.0.3
//	-X github.com/ALRubinger/aileron/core/version.Commit=abc1234
var (
	Version = "dev"
	Commit  = "unknown"
)
