package launch

import "testing"

// PinWorkspaceUIDRemapForTest overrides the workspaceUIDRemapActive gate for
// the duration of the test and restores it on cleanup. The real gate is
// GOOS-dependent (Linux+Docker only), so argv-asserting launcher tests must pin
// it to a known value to stay deterministic across the test runner's OS
// (issue #1461). Exposed to the external launch_test package.
func PinWorkspaceUIDRemapForTest(t *testing.T, active bool) {
	t.Helper()
	orig := workspaceUIDRemapActive
	workspaceUIDRemapActive = func(string) bool { return active }
	t.Cleanup(func() { workspaceUIDRemapActive = orig })
}
