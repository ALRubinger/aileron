//go:build !linux

package spawnhelper

import (
	"fmt"
	"os"
)

// Run on non-Linux platforms is a structured-failure stub. The
// runtime only re-execs into the helper on Linux (per ADR-0014's
// platform sandbox section); seeing the helper marker on another
// OS means the runtime has a wiring bug or the daemon binary is
// being invoked from outside the runtime's spawn path. Either
// way, fail loudly rather than silently passing through.
func Run(encodedReq string) {
	_ = encodedReq
	fmt.Fprintln(os.Stderr, "spawn-helper: invoked on a non-Linux platform; helper is a Linux-only construct (see ADR-0014)")
	os.Exit(1)
}
