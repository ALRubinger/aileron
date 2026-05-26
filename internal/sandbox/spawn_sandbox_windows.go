//go:build windows

package sandbox

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// lowIntegritySID names the Mandatory Integrity Control SID the
// runtime drops the wrapped subprocess down to. The Low integrity
// level is the lowest documented level Win32 user-mode processes
// can run at; it blocks writes to objects labeled Medium or higher
// (almost all user-owned files) per the kernel's Mandatory Access
// Control rules. Per ADR-0014's Windows section.
const lowIntegritySID = "S-1-16-4096"

// Flags for CreateRestrictedToken. Documented in the Windows SDK
// header `winbase.h`; `golang.org/x/sys/windows` does not export
// these constants nor a wrapper for CreateRestrictedToken itself
// at v0.44.0, so the runtime loads them locally and calls the
// advapi32 entry point directly via [createRestrictedToken].
const (
	disableMaxPrivilege uint32 = 0x1 // strip every privilege except SeChangeNotify
	luaToken            uint32 = 0x4 // reset elevation, similar to a fresh standard-user login
)

var (
	modAdvapi32               = windows.NewLazySystemDLL("advapi32.dll")
	procCreateRestrictedToken = modAdvapi32.NewProc("CreateRestrictedToken")
)

// createRestrictedToken wraps the un-exported advapi32 entry point.
// existingToken is the source token to derive from; flags is the
// CreateRestrictedToken flag set (see [disableMaxPrivilege] /
// [luaToken]). The returned handle is a primary token suitable for
// CreateProcessAsUserW. Caller closes via [windows.CloseHandle].
//
// The disable/delete/restrict lists are all nil for v1 (we rely on
// DISABLE_MAX_PRIVILEGE | LUA_TOKEN to do the work). Plumbing them
// through is a v2 hardening: explicit SIDs-to-restrict tightens the
// trust further but isn't load-bearing for the current threat model.
func createRestrictedToken(existing windows.Token, flags uint32) (windows.Token, error) {
	var newToken windows.Token
	r1, _, e1 := procCreateRestrictedToken.Call(
		uintptr(existing),
		uintptr(flags),
		0, 0, // DisableSidCount, SidsToDisable
		0, 0, // DeletePrivilegeCount, PrivilegesToDelete
		0, 0, // RestrictedSidCount, SidsToRestrict
		uintptr(unsafe.Pointer(&newToken)),
	)
	if r1 == 0 {
		if e1 != nil {
			return 0, e1
		}
		return 0, fmt.Errorf("CreateRestrictedToken: unknown failure")
	}
	return newToken, nil
}

// applyPlatformSandbox configures `cmd` to launch the wrapped CLI
// under a restricted token + a job object that will terminate the
// subprocess if the daemon exits.
//
// Scope of this implementation (Windows v1):
//
//   - Restricted token: runtime forks a duplicate of the daemon's
//     own token, calls CreateRestrictedTokenW with
//     DISABLE_MAX_PRIVILEGE | LUA_TOKEN (strips every privilege
//     except SeChangeNotifyPrivilege and resets elevation), then
//     drops the integrity label to Low via SetTokenInformation.
//     The kernel uses this token at CreateProcessAsUserW time via
//     Go's SysProcAttr.Token. The subprocess runs with no ambient
//     privileges and cannot write to objects at Medium integrity
//     or higher.
//   - Job object: an unnamed job with
//     JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE; the spawned process is
//     assigned to the job after Start. Closing the job handle
//     (on cleanup or daemon exit) terminates the subprocess,
//     preventing zombies.
//
// What this does NOT yet deliver:
//
//   - Read confinement. Low integrity blocks writes, not reads.
//     The "no ~/.ssh exfiltration" claim requires AppContainer
//     (ADR-0014 defers).
//   - ACL adjustments on fs_write paths. Low integrity is coarse;
//     per-path ACLs giving the low-integrity token write access
//     to declared scopes are a follow-up.
//
// What this DOES deliver as of Windows v2:
//
//   - Kernel-enforced network egress confinement via WFP filters
//     when the manifest declares [capabilities.network]. A block
//     filter at the ALE_AUTH_CONNECT layer (V4 + V6) denies
//     outbound connect calls from the wrapped binary; a higher-
//     weight permit filter carves out 127.0.0.1:<proxy-port>.
//     Filters are deleted on cleanup; the engine handle close
//     also releases any leftover session-scoped state.
//
// Returns wrapped ErrSpawnUnavailable on token-construction or
// job-creation failures. Returns the zero-value hooks when the
// manifest declares no sandbox params (legacy path).
func applyPlatformSandbox(cmd *exec.Cmd, env SpawnEnvelope, limits SpawnLimits) (platformSandboxHooks, error) {
	if !limits.PlatformSandboxRequested() {
		return platformSandboxHooks{}, nil
	}

	token, err := buildRestrictedToken()
	if err != nil {
		return platformSandboxHooks{}, fmt.Errorf("%w: build restricted token: %v", ErrSpawnUnavailable, err)
	}

	job, err := createSpawnJobObject()
	if err != nil {
		_ = windows.CloseHandle(windows.Handle(token))
		return platformSandboxHooks{}, fmt.Errorf("%w: create job object: %v", ErrSpawnUnavailable, err)
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Token = syscall.Token(token)

	// Network filtering: when the connector declared
	// [capabilities.network] we open a WFP engine session and
	// install per-binary block + loopback-permit filters. The
	// filters identify the wrapped CLI via its FWP_APP_ID (a
	// kernel-computed hash of the binary path) rather than PID,
	// since WFP has no native PID condition and the binary
	// identity is unique per connector for our use case. Concurrent
	// spawns of the same connector binary share filters; this is
	// safe because each spawn's proxy is aileron-managed.
	var (
		netEngine   windows.Handle
		netAppID    *fwpByteBlob
		netFilterIDs []uint64
	)
	if limits.ProxyAddr != "" {
		port, perr := parseProxyPort(limits.ProxyAddr)
		if perr != nil {
			_ = windows.CloseHandle(job)
			_ = windows.CloseHandle(windows.Handle(token))
			return platformSandboxHooks{}, fmt.Errorf("%w: parse proxy address %q: %v",
				ErrSpawnUnavailable, limits.ProxyAddr, perr)
		}
		engine, err := openWFPEngine()
		if err != nil {
			_ = windows.CloseHandle(job)
			_ = windows.CloseHandle(windows.Handle(token))
			return platformSandboxHooks{}, fmt.Errorf("%w: open WFP engine: %v", ErrSpawnUnavailable, err)
		}
		appID, err := getAppID(env.Program)
		if err != nil {
			_ = closeWFPEngine(engine)
			_ = windows.CloseHandle(job)
			_ = windows.CloseHandle(windows.Handle(token))
			return platformSandboxHooks{}, fmt.Errorf("%w: resolve app-id for %s: %v",
				ErrSpawnUnavailable, env.Program, err)
		}
		displayName := "aileron-spawn-" + env.Program
		blockIDs, err := addBlockOutboundForApp(engine, appID, displayName)
		if err != nil {
			freeWFPMemory(unsafe.Pointer(appID))
			_ = closeWFPEngine(engine)
			_ = windows.CloseHandle(job)
			_ = windows.CloseHandle(windows.Handle(token))
			return platformSandboxHooks{}, fmt.Errorf("%w: install WFP block filters: %v",
				ErrSpawnUnavailable, err)
		}
		permitID, err := addPermitLoopbackForApp(engine, appID, port, displayName)
		if err != nil {
			for _, id := range blockIDs {
				_ = deleteFilter(engine, id)
			}
			freeWFPMemory(unsafe.Pointer(appID))
			_ = closeWFPEngine(engine)
			_ = windows.CloseHandle(job)
			_ = windows.CloseHandle(windows.Handle(token))
			return platformSandboxHooks{}, fmt.Errorf("%w: install WFP permit filter: %v",
				ErrSpawnUnavailable, err)
		}
		netEngine = engine
		netAppID = appID
		netFilterIDs = append(append([]uint64(nil), blockIDs...), permitID)
	}

	return platformSandboxHooks{
		PostStart: func(p *os.Process) error {
			return assignProcessToSpawnJob(job, p)
		},
		Cleanup: func() {
			// Tear down WFP filters first so the kernel stops
			// evaluating them; the engine close at the end of
			// this block also auto-removes any session-scoped
			// state that survives.
			if netEngine != 0 {
				for _, id := range netFilterIDs {
					_ = deleteFilter(netEngine, id)
				}
				freeWFPMemory(unsafe.Pointer(netAppID))
				_ = closeWFPEngine(netEngine)
			}
			// CloseHandle on the job releases the kernel object;
			// because of JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, any
			// subprocess still inside the job dies. The token
			// handle was duplicated by the kernel during process
			// create, so closing our copy here is safe.
			_ = windows.CloseHandle(job)
			_ = windows.CloseHandle(windows.Handle(token))
		},
	}, nil
}

// parseProxyPort extracts the port from a host:port string. The
// runtime already validates the proxy listener bound on
// 127.0.0.1; this helper just pulls the number for the WFP
// IP_REMOTE_PORT condition value.
func parseProxyPort(hostPort string) (uint16, error) {
	_, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		return 0, fmt.Errorf("SplitHostPort: %w", err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("parse port %q: %w", portStr, err)
	}
	return uint16(port), nil
}

// buildRestrictedToken duplicates the daemon's primary token,
// strips every privilege except SeChangeNotify, resets elevation
// (LUA_TOKEN), and drops the integrity label to Low. Returns a
// primary token suitable for CreateProcessAsUserW. The caller is
// responsible for closing the handle after exec.Cmd.Start has run
// (the kernel duplicates the token during process create so
// closing our copy is safe).
func buildRestrictedToken() (windows.Token, error) {
	var current windows.Token
	err := windows.OpenProcessToken(
		windows.CurrentProcess(),
		windows.TOKEN_DUPLICATE|windows.TOKEN_ASSIGN_PRIMARY|windows.TOKEN_QUERY|windows.TOKEN_ADJUST_DEFAULT,
		&current,
	)
	if err != nil {
		return 0, fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer current.Close()

	restricted, err := createRestrictedToken(current, disableMaxPrivilege|luaToken)
	if err != nil {
		return 0, fmt.Errorf("CreateRestrictedToken: %w", err)
	}

	if err := setLowIntegrity(restricted); err != nil {
		_ = windows.CloseHandle(windows.Handle(restricted))
		return 0, fmt.Errorf("set low integrity: %w", err)
	}
	return restricted, nil
}

// setLowIntegrity drops the supplied token's integrity label to
// Low (S-1-16-4096). The kernel enforces Mandatory Integrity
// Control: Low subjects cannot write to Medium or High labeled
// objects, which covers nearly all user-owned files on a default
// Windows install.
func setLowIntegrity(token windows.Token) error {
	sid, err := windows.StringToSid(lowIntegritySID)
	if err != nil {
		return fmt.Errorf("StringToSid(%s): %w", lowIntegritySID, err)
	}
	mandatoryLabel := windows.Tokenmandatorylabel{
		Label: windows.SIDAndAttributes{
			Sid:        sid,
			Attributes: windows.SE_GROUP_INTEGRITY,
		},
	}
	return windows.SetTokenInformation(
		token,
		windows.TokenIntegrityLevel,
		(*byte)(unsafe.Pointer(&mandatoryLabel)),
		mandatoryLabel.Size(),
	)
}

// createSpawnJobObject creates an unnamed job object configured
// with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE. Closing the returned
// handle (or daemon exit) terminates any process assigned to the
// job.
func createSpawnJobObject() (windows.Handle, error) {
	handle, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("CreateJobObject: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	_, err = windows.SetInformationJobObject(
		handle,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return 0, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	return handle, nil
}

// assignProcessToSpawnJob opens a handle to the just-started
// subprocess by PID and assigns it to the job object. Called
// from the platformSandboxHooks.PostStart hook so the runtime
// only assigns once the kernel has allocated the process.
//
// Between exec.Cmd.Start and this call the subprocess is live
// but unjobbed for a few microseconds. The window is a known
// trade-off versus CREATE_SUSPENDED + manual ResumeThread which
// Go's os/exec does not directly expose. The brief unjobbed
// window is acceptable for the threat model; documented in
// ADR-0014's Windows section.
func assignProcessToSpawnJob(job windows.Handle, p *os.Process) error {
	procHandle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(p.Pid),
	)
	if err != nil {
		return fmt.Errorf("OpenProcess(%d): %w", p.Pid, err)
	}
	defer windows.CloseHandle(procHandle)
	if err := windows.AssignProcessToJobObject(job, procHandle); err != nil {
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return nil
}
