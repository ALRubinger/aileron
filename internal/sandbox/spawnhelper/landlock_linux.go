//go:build linux

package spawnhelper

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Landlock rule-type constants. ABI v1 ships with only
// PATH_BENEATH (subtree-rooted file-access rules). Newer ABIs
// add NET_PORT and other types; the runtime only uses PATH_BENEATH.
// `golang.org/x/sys/unix` v0.44.0 exposes the access-bit flags but
// not the rule-type number, so the runtime declares it locally.
const landlockRulePathBeneath = 1

// landlockReadAccess is the bitmask of access types this runtime
// declares as "readable" for a manifest's `fs_read` scope. ABI v1
// distinguishes file-read from directory-read; both are needed for
// a wrapped CLI that lists a directory and reads its files.
const landlockReadAccess = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR

// landlockWriteAccess is the bitmask granted to `fs_write` scopes.
// Beyond `WRITE_FILE`, write-scope use cases (caches, output dirs)
// typically also need to create, delete, and rename within the
// subtree. ABI v1 caps the set at REG/CHAR/DIR/SOCK/FIFO/BLOCK/SYM
// makes plus the removes; newer ABIs add TRUNCATE (v3) and REFER
// (v2) which the runtime can extend later.
const landlockWriteAccess = unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
	unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
	unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
	unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
	unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
	unix.LANDLOCK_ACCESS_FS_MAKE_REG |
	unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
	unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
	unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
	unix.LANDLOCK_ACCESS_FS_MAKE_SYM

// applyLandlock creates a Landlock ruleset that governs file
// reads and writes, adds an allow rule per manifest path, sets
// `PR_SET_NO_NEW_PRIVS` so the restriction survives subsequent
// execve calls, and finally restricts the calling process to the
// ruleset. After return, the helper (and the wrapped CLI it
// exec's into) can access only the declared paths.
//
// Returns nil when both lists are empty (no Landlock restriction
// configured — the legacy v1 behavior for connectors without
// FS scopes declared). Returns an error if any syscall fails;
// the helper surfaces this as a non-zero exit so the runtime
// audit row records a sandbox setup failure rather than a
// silently-unconfined spawn.
func applyLandlock(fsRead, fsWrite []string) error {
	if len(fsRead) == 0 && len(fsWrite) == 0 {
		return nil
	}

	// Create the ruleset declaring the access types we'll govern.
	// Once a type is in handled_access_fs, the kernel denies it by
	// default; subsequent add-rule calls re-grant specific subtrees.
	attr := unix.LandlockRulesetAttr{
		Access_fs: landlockReadAccess | landlockWriteAccess,
	}
	rulesetFD, _, errno := unix.Syscall(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)),
		unsafe.Sizeof(attr),
		0,
	)
	if errno != 0 {
		return fmt.Errorf("landlock_create_ruleset: %w", errno)
	}
	defer unix.Close(int(rulesetFD))

	for _, p := range fsRead {
		if err := addLandlockPathRule(int(rulesetFD), p, landlockReadAccess); err != nil {
			return err
		}
	}
	for _, p := range fsWrite {
		// Write scopes get read+write because most write operations
		// open the file for both. Without this, a CLI that writes
		// to a cache directory by reading-modifying-writing fails
		// at the read step.
		if err := addLandlockPathRule(int(rulesetFD), p, landlockReadAccess|landlockWriteAccess); err != nil {
			return err
		}
	}

	// PR_SET_NO_NEW_PRIVS prevents the kernel from re-granting
	// privileges across execve(setuid_binary). Landlock requires
	// this be set before restrict_self per the kernel docs;
	// without it, the call fails with EPERM.
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl(PR_SET_NO_NEW_PRIVS): %w", err)
	}

	_, _, errno = unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, rulesetFD, 0, 0)
	if errno != 0 {
		return fmt.Errorf("landlock_restrict_self: %w", errno)
	}
	return nil
}

// addLandlockPathRule opens `path` as an O_PATH file descriptor
// and adds an allow rule covering it (and everything beneath it,
// per Landlock's `path-beneath` semantics) for the given access
// mask. The FD is closed before return; the kernel retains a
// reference internally.
func addLandlockPathRule(rulesetFD int, path string, access uint64) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("landlock open %q: %w", path, err)
	}
	defer unix.Close(fd)
	pathAttr := unix.LandlockPathBeneathAttr{
		Allowed_access: access,
		Parent_fd:      int32(fd),
	}
	_, _, errno := unix.Syscall6(
		unix.SYS_LANDLOCK_ADD_RULE,
		uintptr(rulesetFD),
		landlockRulePathBeneath,
		uintptr(unsafe.Pointer(&pathAttr)),
		0, 0, 0,
	)
	if errno != 0 {
		return fmt.Errorf("landlock_add_rule %q: %w", path, errno)
	}
	return nil
}

// landlockSupported reports whether the running kernel exposes
// the Landlock API. Returns false on kernels older than 5.13 or
// kernels built without CONFIG_SECURITY_LANDLOCK.
//
// The probe uses `landlock_create_ruleset(NULL, 0, LANDLOCK_CREATE_RULESET_VERSION)`,
// which returns the supported ABI version on success and an
// errno on failure. Caller treats any non-positive return as
// "unsupported."
func landlockSupported() bool {
	r1, _, _ := unix.Syscall(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		0, 0,
		unix.LANDLOCK_CREATE_RULESET_VERSION,
	)
	return int(r1) >= 1
}
