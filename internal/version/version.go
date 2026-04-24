// Package version provides build-time version information for Aileron binaries.
package version

// Set at build time via ldflags:
//
//	-X github.com/ALRubinger/aileron/internal/version.Version=0.0.3
//	-X github.com/ALRubinger/aileron/internal/version.Commit=abc1234
var (
	Version = "dev"
	Commit  = "unknown"
)
