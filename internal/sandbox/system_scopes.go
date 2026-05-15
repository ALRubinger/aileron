package sandbox

import "os"

// existingPaths filters paths to only those present on the
// host. Used by [systemReadScopes] so Landlock's add-rule
// doesn't receive a path the kernel will reject with ENOENT.
// Order is preserved.
func existingPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}
