package spawnhelper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

// RunSpawnShim runs a TCP-to-UDS forwarder until the listener errors
// or the context is canceled. The shim is the Linux-side bridge ADR-0014
// commits to in its "Network confinement: daemon-mediated proxy"
// section: when a spawn connector declares `[capabilities.network]`,
// the daemon's per-spawn CONNECT proxy binds on a Unix-domain socket
// in the host filesystem, the runtime bind-mounts that socket into the
// child's mount namespace, and this shim runs as a sibling of the
// wrapped CLI inside the network namespace to bridge between the CLI's
// expected `127.0.0.1:<port>` and the bind-mounted socket.
//
// The shim has no allowlist logic of its own. Enforcement lives in the
// proxy on the host side ([SpawnProxy]); the shim is dumb plumbing.
// Every byte the CLI writes to the TCP loopback flows to the UDS, and
// vice versa. CONNECT-method parsing, host:port allowlisting, and
// audit emission happen at the proxy.
//
// Lifecycle:
//
//   - `tcpAddr` is the listen address on the namespace's loopback,
//     typically `127.0.0.1:0` (ephemeral). The function publishes the
//     resolved address on `readyAddr` when the listener is bound; the
//     caller drains the channel to learn what `HTTPS_PROXY` value to
//     set on the wrapped CLI.
//   - The shim accepts connections in a loop and spawns a forwarding
//     goroutine per connection. Forwarding goroutines exit when either
//     side closes (EOF or io.ErrClosedPipe).
//   - On `ctx` cancellation the listener is closed and Run returns
//     after in-flight forwards drain. Returns `ctx.Err()` for cancel,
//     or the underlying accept/listen error otherwise. A clean shutdown
//     from listener close returns nil.
//
// Although ADR-0014 frames the shim as Linux-only (it pairs with the
// network namespace setup), the implementation itself uses only
// portable `net` primitives so unit tests can exercise it on any
// platform.
func RunSpawnShim(ctx context.Context, tcpAddr, udsPath string, readyAddr chan<- string) error {
	if tcpAddr == "" {
		return errors.New("spawn shim: tcpAddr is required")
	}
	if udsPath == "" {
		return errors.New("spawn shim: udsPath is required")
	}

	ln, err := net.Listen("tcp", tcpAddr)
	if err != nil {
		return fmt.Errorf("spawn shim: listen %s: %w", tcpAddr, err)
	}
	defer ln.Close()

	if readyAddr != nil {
		select {
		case readyAddr <- ln.Addr().String():
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Track in-flight client connections so context cancel can force
	// them closed. io.Copy does not honor a context; the only way to
	// unblock a long-lived tunnel on cancel is to close its underlying
	// connection. Without this, an idle CLI holding a CONNECT tunnel
	// open would pin the shim past ctx.Done.
	var (
		mu      sync.Mutex
		clients = map[net.Conn]struct{}{}
	)
	register := func(c net.Conn) {
		mu.Lock()
		clients[c] = struct{}{}
		mu.Unlock()
	}
	unregister := func(c net.Conn) {
		mu.Lock()
		delete(clients, c)
		mu.Unlock()
	}

	go func() {
		<-ctx.Done()
		_ = ln.Close()
		mu.Lock()
		for c := range clients {
			_ = c.Close()
		}
		mu.Unlock()
	}()

	var wg sync.WaitGroup
	for {
		client, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				wg.Wait()
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				return nil
			}
			wg.Wait()
			return fmt.Errorf("spawn shim: accept: %w", err)
		}
		register(client)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer unregister(client)
			forwardOne(client, udsPath)
		}()
	}
}

// forwardOne dials the UDS and pipes bytes between the client TCP
// connection and the Unix socket. Returns when either side closes.
//
// Dial errors are surfaced by closing the client connection without
// forwarding; the upstream proxy was unreachable, the CLI sees a
// closed TCP connection, and any HTTP CONNECT in flight on the client
// fails with the standard "connection closed unexpectedly" shape.
// The proxy is the audit emission point, not the shim.
func forwardOne(client net.Conn, udsPath string) {
	defer client.Close()
	upstream, err := net.Dial("unix", udsPath)
	if err != nil {
		return
	}
	defer upstream.Close()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, client)
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
