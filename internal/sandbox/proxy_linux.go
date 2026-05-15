//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
)

// defaultStartSpawnProxy on Linux binds the per-spawn CONNECT
// proxy on a Unix-domain socket under `$XDG_RUNTIME_DIR` (or
// `/tmp` when `$XDG_RUNTIME_DIR` is unset). The wrapped CLI runs
// inside a network namespace (`CLONE_NEWNET`) where the host's
// TCP loopback is unreachable; the in-namespace [spawnhelper.Run]
// bridges from a namespace-local TCP loopback to this UDS via
// [RunSpawnShim].
//
// Per-spawn UDS paths are removed when the proxy closes so the
// socket file doesn't linger after the spawn completes.
func defaultStartSpawnProxy(ctx context.Context, policy *HostPolicy, logger *slog.Logger, fqn string) (*SpawnProxy, ProxyEndpoint, func(), error) {
	udsPath, err := makeProxyUDSPath()
	if err != nil {
		return nil, ProxyEndpoint{}, nil, fmt.Errorf("spawn proxy: pick UDS path: %w", err)
	}
	// Pre-emptively remove a stale socket if one exists; net.Listen
	// fails with "address already in use" otherwise.
	_ = os.Remove(udsPath)

	ln, err := net.Listen("unix", udsPath)
	if err != nil {
		return nil, ProxyEndpoint{}, nil, fmt.Errorf("spawn proxy: listen unix %s: %w", udsPath, err)
	}
	// Permissions: only the daemon's user should reach the socket;
	// the wrapped CLI runs as the same user inside the namespace
	// so 0600 (or 0700 on the parent dir) is enough.
	if err := os.Chmod(udsPath, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(udsPath)
		return nil, ProxyEndpoint{}, nil, fmt.Errorf("spawn proxy: chmod UDS: %w", err)
	}

	proxy := NewSpawnProxy(policy, logger, fqn)
	go func() {
		_ = proxy.Serve(ctx, ln)
	}()
	endpoint := ProxyEndpoint{UDSPath: udsPath}
	closeFn := func() {
		_ = proxy.Close()
		// Best-effort cleanup. If the daemon crashed between
		// listener creation and CloseFn the socket file lingers;
		// the next spawn picks a different ephemeral name so the
		// stale file is harmless.
		_ = os.Remove(udsPath)
	}
	return proxy, endpoint, closeFn, nil
}

// makeProxyUDSPath returns a path under the user's runtime dir
// where the proxy can bind a per-spawn UDS. Prefers
// `$XDG_RUNTIME_DIR` (typically `/run/user/<uid>` on systemd
// hosts), falls back to `/tmp` so the path stays short enough
// for sockaddr_un (108 chars on Linux).
//
// Path shape: `<dir>/aileron-proxy-<pid>-<random>.sock`. The
// per-spawn random suffix avoids collisions when the daemon
// runs concurrent invocations.
func makeProxyUDSPath() (string, error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = "/tmp"
	}
	f, err := os.CreateTemp(dir, "aileron-proxy-*.sock")
	if err != nil {
		return "", fmt.Errorf("create temp in %s: %w", dir, err)
	}
	path := f.Name()
	_ = f.Close()
	// Remove the empty file; net.Listen creates the socket.
	_ = os.Remove(path)
	if got := len(path); got > 107 {
		// Linux sockaddr_un.sun_path is 108 bytes including NUL.
		// If our dir choice produces too long a path the listener
		// will fail with EINVAL.
		return "", fmt.Errorf("UDS path %q too long for sockaddr_un (108 bytes)", filepath.Base(path))
	}
	return path, nil
}
