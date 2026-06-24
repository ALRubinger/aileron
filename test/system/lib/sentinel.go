// Per-run sentinel generation for the R9 deterministic-result assertion. The
// bash scenario derives a fresh run id from `date +%s`-`$$` and forms the
// sentinel `AILERON_SYSTEST_OK_<runid>`, so a stale workspace file from a prior
// run can never pass the byte-exact check. This file re-expresses that as pure
// Go: NewRunID produces a fresh, filename-safe id from the wall clock, the PID,
// and a random component (the Go analogues of `date +%s` and `$$`), and Sentinel
// forms the token. Pure (no filesystem, no docker); the live driver writes
// nothing here and only reads the host-side file the agent produced.
package systestlib

import (
	"fmt"
	"math/rand"
	"os"
	"time"
)

// sentinelPrefix is the fixed token prefix the agent writes, matching codex.sh's
// `SENTINEL_EXPECTED="AILERON_SYSTEST_OK_${RUNID}"`.
const sentinelPrefix = "AILERON_SYSTEST_OK_"

// NewRunID returns a fresh, filename-safe run id unique per call. It mirrors the
// bash `date +%s`-`$$` (seconds + PID) and adds a random suffix so two calls
// within the same second and process still differ (the bash relies on the
// process being fresh per run; the Go driver is one process, so the random
// component supplies the freshness the PID gave bash). The charset is
// [0-9a-z-] only, so the id is safe in a filename and in the shell prompt.
func NewRunID() string {
	now := time.Now().UnixNano()
	pid := os.Getpid()
	// rand.Int63 is seeded from the runtime's global source (auto-seeded since
	// Go 1.20), which is sufficient for de-duplication; this is not a security
	// token. Mask to a positive value and format base-36 for a compact charset.
	suffix := rand.Int63()
	if suffix < 0 {
		suffix = -suffix
	}
	return fmt.Sprintf("%d-%d-%s", now, pid, strconvBase36(suffix))
}

// Sentinel forms the per-run sentinel string `AILERON_SYSTEST_OK_<runID>` the
// agent is instructed to write and the host-side check compares byte-exact.
func Sentinel(runID string) string {
	return sentinelPrefix + runID
}

// strconvBase36 formats a non-negative int64 in base-36 ([0-9a-z]) without
// importing strconv's FormatInt purely to keep the call site explicit about the
// filename-safe charset guarantee.
func strconvBase36(n int64) string {
	if n == 0 {
		return "0"
	}
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	var buf [13]byte // enough for max int64 in base 36
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%36]
		n /= 36
	}
	return string(buf[i:])
}
