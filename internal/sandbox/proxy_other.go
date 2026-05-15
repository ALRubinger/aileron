//go:build !linux

package sandbox

import (
	"context"
	"log/slog"
	"net"
)

// defaultStartSpawnProxy on non-Linux platforms binds the
// per-spawn CONNECT proxy on TCP loopback. The platform sandbox
// (macOS [SBPL] / Windows WFP) permits the wrapped CLI to reach
// `127.0.0.1:<port>` directly, so the runtime sets `HTTPS_PROXY`
// to the returned [ProxyEndpoint.TCPAddr] and the CLI dials
// straight through.
//
// [SBPL]: see internal/sandbox/spawn_sandbox_darwin.go
func defaultStartSpawnProxy(ctx context.Context, policy *HostPolicy, logger *slog.Logger, fqn string) (*SpawnProxy, ProxyEndpoint, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, ProxyEndpoint{}, nil, err
	}
	proxy := NewSpawnProxy(policy, logger, fqn)
	go func() {
		// Serve blocks until Close is called or the listener
		// errors. Per-connection errors are surfaced via audit
		// emission inside serveConn; this goroutine's return
		// value is intentionally dropped because Close is the
		// normal termination path.
		_ = proxy.Serve(ctx, ln)
	}()
	endpoint := ProxyEndpoint{TCPAddr: ln.Addr().String()}
	closeFn := func() {
		_ = proxy.Close()
	}
	return proxy, endpoint, closeFn, nil
}
