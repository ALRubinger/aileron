// Pure parsing for the daemon-lifecycle CLI calls the scenario driver makes
// (`aileron daemon start/status/stop`). Kept here, in the GO_TEST_PACKAGES lib,
// so the parse logic is unit-tested in CI while the impure exec.Command plumbing
// stays in the by-hand scenario driver.
package systestlib

import "strings"

// ParseDaemonStartURL extracts the daemon URL from `aileron daemon start` output,
// whose success line is "Aileron daemon running at <url>" (idempotent — it prints
// the same line for an already-running daemon). Returns "" if no such line is
// present.
func ParseDaemonStartURL(out string) string {
	const marker = "running at "
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, marker); i >= 0 {
			return strings.TrimSpace(line[i+len(marker):])
		}
	}
	return ""
}

// DaemonStatusRunning reports whether `aileron daemon status` output indicates a
// running daemon. Status prints "Aileron daemon is running." when up and
// "Aileron daemon is not running." when down; the substring "daemon is running"
// matches only the former (the latter contains "daemon is not running").
func DaemonStatusRunning(out string) bool {
	return strings.Contains(out, "daemon is running")
}
