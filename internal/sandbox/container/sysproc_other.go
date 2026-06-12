//go:build windows

package container

import "os/exec"

// setRuntimeChildPgid is a no-op on Windows. Setpgid is a Unix-only
// SysProcAttr field, and sandbox container launch is a Linux/macOS dev
// path; Windows uses a different process-teardown model (job objects /
// CTRL_BREAK), so the terminal-SIGINT-to-process-group race the Unix
// helper guards against does not apply. The no-op keeps the package
// building on Windows. See ADR-0025 and issue #999.
func setRuntimeChildPgid(_ *exec.Cmd) {}
