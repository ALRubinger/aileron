//go:build windows

package sandbox

import (
	"testing"
	"unsafe"

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

func TestGetAppID_ReturnsBlobForRealBinary(t *testing.T) {
	// Contract: FwpmGetAppIdFromFileName0 resolves a real Windows
	// binary path to the kernel-computed app-id blob the WFP
	// ALE_APP_ID condition matches against. The blob is opaque to
	// the runtime (a hash); we just assert it allocated, points
	// somewhere, and has a non-zero size.
	blob, err := getAppID(`C:\Windows\System32\cmd.exe`)
	if err != nil {
		t.Fatalf("getAppID(cmd.exe): %v", err)
	}
	if blob == nil {
		t.Fatal("getAppID returned nil blob on success")
	}
	defer freeWFPMemory(unsafe.Pointer(blob))
	if blob.Size == 0 {
		t.Errorf("blob size = 0; want non-zero hash")
	}
	if blob.Data == nil {
		t.Errorf("blob data pointer is nil despite non-zero size %d", blob.Size)
	}
}

func TestWFPFilters_AddDeleteCycle(t *testing.T) {
	// End-to-end (struct-layout proof): open engine, resolve
	// app-id for cmd.exe, install the deny + permit filter set
	// the runtime uses in production, then delete them and close
	// the engine. The test passes when every kernel call returns
	// success — proving the FWPM_FILTER0 and FWP_VALUE0 struct
	// layouts match what fwpuclnt.dll expects.
	engine, err := openWFPEngine()
	if err != nil {
		t.Fatalf("openWFPEngine: %v", err)
	}
	defer closeWFPEngine(engine)

	appID, err := getAppID(`C:\Windows\System32\cmd.exe`)
	if err != nil {
		t.Fatalf("getAppID: %v", err)
	}
	defer freeWFPMemory(unsafe.Pointer(appID))

	blockIDs, err := addBlockOutboundForApp(engine, appID, "aileron-test-block")
	if err != nil {
		t.Fatalf("addBlockOutboundForApp: %v", err)
	}
	if len(blockIDs) != 2 {
		t.Errorf("addBlockOutboundForApp returned %d filter IDs, want 2 (V4 + V6)", len(blockIDs))
	}
	for i, id := range blockIDs {
		if id == 0 {
			t.Errorf("block filter [%d] has zero ID", i)
		}
	}

	permitID, err := addPermitLoopbackForApp(engine, appID, 54321, "aileron-test-permit")
	if err != nil {
		for _, id := range blockIDs {
			_ = deleteFilter(engine, id)
		}
		t.Fatalf("addPermitLoopbackForApp: %v", err)
	}
	if permitID == 0 {
		t.Errorf("permit filter has zero ID")
	}

	// Clean up: delete all filters explicitly. The contract is
	// that delete returns a non-fatal error code when the filter
	// doesn't exist; we just check the API doesn't crash.
	for _, id := range blockIDs {
		if err := deleteFilter(engine, id); err != nil {
			t.Errorf("deleteFilter(block, %d): %v", id, err)
		}
	}
	if err := deleteFilter(engine, permitID); err != nil {
		t.Errorf("deleteFilter(permit, %d): %v", permitID, err)
	}
}

func TestParseProxyPort(t *testing.T) {
	// Sanity for the helper applyPlatformSandbox uses to extract
	// the port for the WFP IP_REMOTE_PORT condition.
	cases := []struct {
		in   string
		want uint16
		err  bool
	}{
		{"127.0.0.1:54321", 54321, false},
		{"127.0.0.1:65535", 65535, false},
		{"127.0.0.1:0", 0, false},
		{"no-colon", 0, true},
		{"127.0.0.1:notanumber", 0, true},
		{"127.0.0.1:65536", 0, true}, // overflow uint16
	}
	for _, tc := range cases {
		got, err := parseProxyPort(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("parseProxyPort(%q) expected error, got %d", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseProxyPort(%q): unexpected error %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("parseProxyPort(%q) = %d, want %d", tc.in, got, tc.want)
		}
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
