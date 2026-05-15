//go:build linux

package sandbox

// systemReadScopes returns the host filesystem paths a Linux
// subprocess needs to read in order to start under Landlock:
// dynamic linker (`/lib`, `/lib64`), shared libraries (`/usr`),
// configuration that libc consults (`/etc`), and the standard
// system binaries (`/bin`) that shell shebangs resolve to.
//
// These are the same paths the platform sandbox's integration
// tests grant — they're load-bearing for any wrapped program
// that links against libc or invokes `/bin/sh` (every shell
// script does). Without them, Landlock denies the dynamic
// linker's open of libc and the wrapped CLI fails to start
// with `permission denied`, even when the manifest's
// fs_read scope correctly covers the binary itself.
//
// Returns only paths that exist on the host so the caller does
// not feed Landlock a nonexistent path (which would fail the
// add-rule with ENOENT). Non-Linux platforms return nil; their
// sandbox profiles (sandbox-exec on macOS, restricted token +
// job object on Windows) include system paths in their default
// permissive baselines and do not need this augmentation.
func systemReadScopes() []string {
	return existingPaths([]string{
		"/lib",
		"/lib64",
		"/usr",
		"/etc",
		"/bin",
	})
}
