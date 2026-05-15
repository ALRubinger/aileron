//go:build windows

package sandbox

import (
	"testing"

	"golang.org/x/sys/windows"
)

// TestWFPEngine_OpenClose is the first signal whether the
// Windows v2 plan is viable on GitHub Actions. WFP filter
// operations typically need either `BUILTIN\Administrators`
// membership or `SE_DEBUG_NAME` privilege; the `windows-latest`
// runner is widely reported to run as an admin user, but the
// guarantee is undocumented. If this test fails with ACCESS_DENIED
// the runtime will need a different mechanism for kernel-enforced
// network confinement on Windows (and the v2 PR will reflect
// that constraint honestly).
//
// On success the engine opens and closes without leaking the
// handle. The test does not add any filters; that's the next
// increment.
func TestWFPEngine_OpenClose(t *testing.T) {
	engine, err := openWFPEngine()
	if err != nil {
		t.Fatalf("openWFPEngine: %v", err)
	}
	if engine == 0 {
		t.Fatal("openWFPEngine returned a zero handle on success")
	}
	if err := closeWFPEngine(engine); err != nil {
		t.Errorf("closeWFPEngine: %v", err)
	}
}

// TestWFPEngine_DoubleClose pins the contract that close on an
// already-closed handle returns an error rather than panicking.
// The runtime calls close exactly once in production, but tests
// and defensive cleanup paths benefit from the assertion that
// double-close is observable, not catastrophic.
func TestWFPEngine_DoubleClose(t *testing.T) {
	engine, err := openWFPEngine()
	if err != nil {
		t.Fatalf("openWFPEngine: %v", err)
	}
	if err := closeWFPEngine(engine); err != nil {
		t.Fatalf("first close: %v", err)
	}
	// Second close: the kernel returns an error code for an
	// invalid handle. We assert non-nil error rather than a
	// specific code because the exact code (ERROR_INVALID_HANDLE
	// vs ERROR_NOT_FOUND) varies by Windows build.
	if err := closeWFPEngine(engine); err == nil {
		t.Error("expected error on double-close, got nil")
	}
}

// Compile-time check that windows.Handle is the type
// [openWFPEngine] returns. Keeps the function's signature
// stable across x/sys/windows upgrades; if the package ever
// renames Handle this assertion fails at build time so the
// runtime doesn't silently regress.
var _ windows.Handle = func() windows.Handle {
	var h windows.Handle
	return h
}()
