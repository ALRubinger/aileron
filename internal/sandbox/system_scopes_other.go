//go:build !linux

package sandbox

// systemReadScopes is a no-op on non-Linux platforms. macOS's
// sandbox-exec profile and Windows' restricted-token model both
// supply system-path access through their permissive baselines;
// the runtime does not need to enumerate them explicitly.
//
// See system_scopes_linux.go for the load-bearing case.
func systemReadScopes() []string {
	return nil
}
