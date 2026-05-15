//go:build windows

// Package-level WFP (Windows Filtering Platform) glue for the
// Windows v2 spawn sandbox. WFP is the kernel-level packet
// filtering framework the runtime uses to deny outbound traffic
// from a wrapped CLI's process to any destination other than
// the daemon's per-spawn CONNECT proxy on loopback.
//
// `golang.org/x/sys/windows` v0.44.0 ships no WFP wrappers, so
// this file loads `fwpuclnt.dll` via [windows.NewLazySystemDLL]
// and calls the C-level entry points directly via Syscall. The
// struct layouts match the Win32 SDK headers (`fwpmtypes.h` and
// `fwptypes.h`); changes to those headers between SDK versions
// could in principle break alignment, but the relevant types
// are stable across the Windows 7+ era.
//
// Scope of this file (Windows v2 first cut):
//
//   - [openWFPEngine] / [closeWFPEngine] manage a session-scoped
//     engine handle. Filters added via the session are
//     automatically removed when the handle closes, so daemon
//     crashes don't leak persistent rules.
//   - Filter add / delete and condition construction land in
//     follow-up work once the engine-open path is validated on
//     the GHA `windows-latest` runner. The probe is the first
//     real signal that WFP is usable without elevated privileges
//     in CI.

package sandbox

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modFwpuclnt           = windows.NewLazySystemDLL("fwpuclnt.dll")
	procFwpmEngineOpen0   = modFwpuclnt.NewProc("FwpmEngineOpen0")
	procFwpmEngineClose0  = modFwpuclnt.NewProc("FwpmEngineClose0")
)

// RPC authentication service constants from `rpcdce.h`. WFP's
// engine-open accepts these for the RPC channel it sets up to
// the local filter engine; `RPC_C_AUTHN_WINNT` is the documented
// default for local connections.
const (
	rpcCAuthnDefault uint32 = 0xFFFFFFFF
	rpcCAuthnWinNT   uint32 = 10
)

// openWFPEngine opens a default-session handle to the local
// Windows Filter Engine. Returns the engine handle (caller must
// close via [closeWFPEngine]) or an error if the open failed.
//
// Failure modes worth distinguishing in the caller:
//
//   - `ERROR_ACCESS_DENIED` (0x5): the daemon process lacks
//     `SE_DEBUG_NAME` or membership in the BUILTIN\Administrators
//     group required for filter-engine access on most Windows
//     SKUs. Surface as wrapped `ErrSpawnUnavailable` so the
//     audit row records the structured class.
//   - `RPC_S_SERVER_UNAVAILABLE` (0x6BA): Base Filtering Engine
//     service is not running. Same surface.
//
// Any other error indicates a misconfiguration we want loud.
func openWFPEngine() (windows.Handle, error) {
	var engine windows.Handle
	r1, _, _ := procFwpmEngineOpen0.Call(
		0, // serverName: NULL for local
		uintptr(rpcCAuthnWinNT),
		0, // authIdentity: NULL
		0, // session: NULL (default)
		uintptr(unsafe.Pointer(&engine)),
	)
	if r1 != 0 {
		return 0, fmt.Errorf("FwpmEngineOpen0: error code 0x%x", r1)
	}
	return engine, nil
}

// closeWFPEngine releases a handle returned by [openWFPEngine].
// Must be called exactly once per opened handle: the Win32 API
// does not validate that the handle is still live, and a
// double-close typically crashes the host process with an
// access violation rather than returning an error. Production
// callers pair Open/Close in a defer.
func closeWFPEngine(engine windows.Handle) error {
	r1, _, _ := procFwpmEngineClose0.Call(uintptr(engine))
	if r1 != 0 {
		return fmt.Errorf("FwpmEngineClose0: error code 0x%x", r1)
	}
	return nil
}
