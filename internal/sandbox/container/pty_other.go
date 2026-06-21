//go:build windows

package container

import (
	"errors"
	"os/exec"
)

// errRunPTYUnsupported signals the caller to fall through to the plain
// stdio exec path. On Windows aileron does not allocate a Unix PTY; the
// console model differs (job objects / CTRL_BREAK) and sandbox launch is a
// Linux/macOS dev path. See issue #1029.
var errRunPTYUnsupported = errors.New("container: PTY ownership is not supported on this platform")

// runRuntimeChildPTY is a no-op on Windows: it always reports unsupported
// so execRunner.Run uses the plain stdio path. See issue #1029.
func runRuntimeChildPTY(_ *exec.Cmd) error { return errRunPTYUnsupported }
