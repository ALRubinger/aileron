package sandbox

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ALRubinger/aileron/internal/audit"
)

// SpawnProxy is the per-invocation HTTP CONNECT proxy that mediates a
// spawned subprocess's outbound network. The proxy is the single
// enforcement seam for `[capabilities.network]` on spawn connectors
// (per ADR-0014's "Network confinement: daemon-mediated proxy"
// section). One proxy is created per spawn call, served on a
// local-only listener the runtime hands in (TCP loopback on
// macOS/Windows; Unix-domain socket on Linux), then closed when the
// spawn completes.
//
// The proxy operates at the CONNECT method only. It does not
// terminate TLS, so it sees host:port plus tunneled bytes but never
// the request URL, headers, or body. Allowlist evaluation reuses
// [HostPolicy] from the WASM network gate; both gates check against
// the same `host:port` set.
type SpawnProxy struct {
	policy *HostPolicy
	logger *slog.Logger
	fqn    string

	dial   func(ctx context.Context, network, addr string) (net.Conn, error)
	closed atomic.Bool

	mu sync.Mutex
	ln net.Listener
}

// NewSpawnProxy constructs a proxy that enforces `policy` and emits
// audit attributes naming `fqn`. The proxy is not started until
// [SpawnProxy.Serve] is called. `logger` is optional; nil suppresses
// audit emission (useful in tests).
func NewSpawnProxy(policy *HostPolicy, logger *slog.Logger, fqn string) *SpawnProxy {
	return &SpawnProxy{
		policy: policy,
		logger: logger,
		fqn:    fqn,
		dial:   defaultProxyDial,
	}
}

// defaultProxyDial is the production dialer. Tests substitute a fake
// via [SpawnProxy.SetDialer] so they can drive the proxy without
// real upstream sockets.
func defaultProxyDial(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	d.Timeout = 10 * time.Second
	return d.DialContext(ctx, network, addr)
}

// SetDialer overrides the function the proxy uses to reach upstream
// hosts. The runtime never calls this; tests inject fakes to avoid
// dialing real network endpoints. Must be called before [Serve].
func (p *SpawnProxy) SetDialer(dial func(ctx context.Context, network, addr string) (net.Conn, error)) {
	p.dial = dial
}

// Serve accepts connections on `ln` until the listener returns
// [net.ErrClosed] (typically via [SpawnProxy.Close]). Each accepted
// connection is handled in its own goroutine. Serve blocks until the
// listener is closed and all in-flight CONNECTs have drained.
//
// Returns nil on a clean shutdown via Close; returns the underlying
// accept error otherwise.
func (p *SpawnProxy) Serve(ctx context.Context, ln net.Listener) error {
	p.mu.Lock()
	if p.closed.Load() {
		p.mu.Unlock()
		ln.Close()
		return nil
	}
	p.ln = ln
	p.mu.Unlock()

	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			if p.closed.Load() || errors.Is(err, net.ErrClosed) {
				wg.Wait()
				return nil
			}
			wg.Wait()
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.serveConn(ctx, conn)
		}()
	}
}

// Close shuts down the proxy. Outstanding CONNECTs are allowed to
// finish; new accepts unblock with [net.ErrClosed]. Safe to call
// from any goroutine. Idempotent.
func (p *SpawnProxy) Close() error {
	p.closed.Store(true)
	p.mu.Lock()
	ln := p.ln
	p.mu.Unlock()
	if ln == nil {
		return nil
	}
	return ln.Close()
}

// serveConn handles a single client connection: read the CONNECT
// line, check the allowlist, dial upstream, tunnel bytes both ways.
//
// On allowlist miss, returns `403 Forbidden`. On upstream-dial
// failure, returns `502 Bad Gateway`. On non-CONNECT methods, returns
// `405 Method Not Allowed`. Every decision (allow, deny, error)
// emits one audit row.
func (p *SpawnProxy) serveConn(ctx context.Context, client net.Conn) {
	defer client.Close()
	if dl, ok := ctx.Deadline(); ok {
		client.SetDeadline(dl)
	}

	reader := bufio.NewReader(client)
	req, err := http.ReadRequest(reader)
	if err != nil {
		p.emitAudit(ctx, "", "deny", "malformed_request")
		return
	}
	defer func() {
		// Drain the body so subsequent reads on the same connection
		// (none in CONNECT, but http.Request reserves the body) don't
		// hang.
		_ = req.Body.Close()
	}()

	if req.Method != http.MethodConnect {
		p.emitAudit(ctx, req.Host, "deny", "method_not_allowed")
		writeStatus(client, http.StatusMethodNotAllowed, "CONNECT required")
		return
	}

	target := req.Host
	if target == "" {
		p.emitAudit(ctx, "", "deny", "malformed_request")
		writeStatus(client, http.StatusBadRequest, "missing host")
		return
	}

	if err := p.policy.CheckHostPort(target); err != nil {
		p.emitAudit(ctx, target, "deny", "capability_denied")
		writeStatus(client, http.StatusForbidden, "host not in allowlist")
		return
	}

	upstream, err := p.dial(ctx, "tcp", target)
	if err != nil {
		p.emitAudit(ctx, target, "deny", "upstream_unreachable")
		writeStatus(client, http.StatusBadGateway, "upstream dial failed")
		return
	}
	defer upstream.Close()

	p.emitAudit(ctx, target, "allow", "")

	if _, err := client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		return
	}

	// Bidirectional tunnel. Drain whatever the client buffered into
	// reader as part of the request parse; without this the first
	// bytes of the TLS handshake would be stuck in the bufio buffer.
	if n := reader.Buffered(); n > 0 {
		buffered, _ := reader.Peek(n)
		if _, err := upstream.Write(buffered); err != nil {
			return
		}
		_, _ = reader.Discard(n)
	}

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, client)
		// Half-close upstream so the upstream peer sees EOF and we
		// don't leak goroutines on long-lived idle tunnels.
		if cw, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		if cw, ok := client.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

// emitAudit logs a structured `spawn_network` event per CONNECT
// request: connector identity, target host:port, decision, deny class.
// Mirrors emitSpawnAudit's shape so operators can correlate spawn
// invocations with their network calls.
func (p *SpawnProxy) emitAudit(ctx context.Context, target, decision, denyClass string) {
	if p.logger == nil {
		return
	}
	attrs := []slog.Attr{
		slog.String("connector", p.fqn),
		slog.String("host_port", target),
		slog.String(audit.AttrPolicyDecision, decision),
	}
	if denyClass != "" {
		attrs = append(attrs, slog.String("deny_class", denyClass))
	}
	p.logger.LogAttrs(ctx, slog.LevelInfo, "spawn_network", attrs...)
}

// writeStatus writes a minimal HTTP/1.1 response with the given
// status code and a one-line reason phrase. The body is empty; the
// reason phrase is informational only and not inspected by typical
// HTTPS proxy clients.
func writeStatus(w io.Writer, code int, reason string) {
	statusLine := "HTTP/1.1 " + http.StatusText(code) + "\r\n"
	if code != 0 {
		statusLine = "HTTP/1.1 " + itoa(code) + " " + http.StatusText(code) + "\r\n"
	}
	_, _ = w.Write([]byte(statusLine + "Content-Length: 0\r\n\r\n"))
	_ = reason // reserved for future structured response body
}

// ProxyEndpoint describes where a per-spawn CONNECT proxy is
// listening. macOS and Windows ([SBPL] / [WFP]) permit the wrapped
// CLI to reach a TCP loopback port directly, so [TCPAddr] is the
// `HTTPS_PROXY` value the runtime sets on the subprocess. Linux
// puts the wrapped CLI in a new network namespace under
// `CLONE_NEWNET`, where the host's TCP loopback is unreachable;
// the proxy binds on a Unix-domain socket whose path is in
// [UDSPath], and the in-namespace spawn helper bridges TCP from
// inside the namespace to that socket.
//
// Exactly one of TCPAddr or UDSPath is populated for a given
// platform. The runtime's [processSpawn] selects which to set
// on the wrapped CLI's environment vs. on the helper request
// based on which field is non-empty.
type ProxyEndpoint struct {
	// TCPAddr is the host:port the proxy is listening on for
	// non-Linux platforms. Empty on Linux.
	TCPAddr string

	// UDSPath is the host filesystem path of the Unix-domain
	// socket the proxy is listening on for Linux. Empty on
	// non-Linux platforms.
	UDSPath string
}

// startSpawnProxy stands up a per-spawn CONNECT proxy on the
// platform-appropriate listener and returns a [ProxyEndpoint]
// describing where the subprocess (or helper bridge) should
// reach it. The returned `closeFn` tears the proxy down (closes
// the listener, drains in-flight tunnels, removes the UDS file
// when applicable). Tests substitute via overriding this var.
//
// Errors are construct-time setup only: a listener bind failure
// or an invalid host policy. Per-CONNECT errors do not propagate
// here; they emit audit rows and the subprocess sees an HTTP
// status.
//
// Per-platform implementation lives in proxy_linux.go (UDS)
// and proxy_other.go (TCP loopback). The var indirection is for
// test injection.
var startSpawnProxy = defaultStartSpawnProxy

// itoa avoids strconv just to keep the proxy's import surface small.
// Status codes are always three ASCII digits in the supported range.
func itoa(n int) string {
	if n < 100 || n > 999 {
		return "000"
	}
	return string([]byte{'0' + byte(n/100), '0' + byte((n/10)%10), '0' + byte(n%10)})
}
