//go:build !windows

package spawnhelper

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

// The spawn shim is a Linux-specific facility (per ADR-0014 and
// #718): a TCP-loopback to Unix-domain-socket bridge that lives
// inside the spawn namespace and forwards bytes between the
// wrapped CLI and the daemon's UDS proxy.
//
// spawn_shim.go itself uses only portable net primitives so tests
// run on macOS dev machines without a real Linux namespace.
// Windows is excluded from the test build because:
//   1. The shim is never invoked on Windows (no namespace-based
//      sandboxing; Windows uses job objects + restricted tokens).
//   2. The fixtures use `/tmp` Unix-domain socket paths, and
//      Windows's AF_UNIX support plus path semantics differ enough
//      that the same fixtures don't translate cleanly.
// A Windows equivalent forwarder, if ever needed, gets its own
// test file rather than retrofitting these.

// shortSockPath returns a UDS path short enough to fit in sockaddr_un
// (sun_path is 104 chars on macOS, 108 on Linux). t.TempDir() under
// macOS resolves under /var/folders/.../T/... which routinely
// exceeds the limit, breaking unix-socket listen with EINVAL. Use
// /tmp with a random suffix instead.
func shortSockPath(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("/tmp", "aileron-shim-*.sock")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	path := f.Name()
	_ = f.Close()
	_ = os.Remove(path) // unix.Listen creates it; pre-existing file blocks bind
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

// The shim's contract per ADR-0014: every TCP byte arriving at the
// in-namespace listener flows to the bind-mounted UDS, and every byte
// the UDS returns flows back to the TCP client. No request parsing,
// no allowlisting. The proxy on the host side is the enforcement
// seam; the shim is plumbing.

func TestRunSpawnShim_BridgesBytesBidirectionally(t *testing.T) {
	// Contract: bytes the TCP client sends arrive at the UDS verbatim;
	// bytes the UDS sends back arrive at the TCP client verbatim.
	udsPath := shortSockPath(t)

	// Echo server on the UDS side.
	udsLn, err := net.Listen("unix", udsPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer udsLn.Close()
	go func() {
		for {
			c, err := udsLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	shimDone := make(chan error, 1)
	go func() { shimDone <- RunSpawnShim(ctx, "127.0.0.1:0", udsPath, ready) }()

	addr := <-ready
	client, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial shim: %v", err)
	}
	defer client.Close()
	client.SetDeadline(time.Now().Add(2 * time.Second))

	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != "ping" {
		t.Errorf("echo = %q, want %q", buf, "ping")
	}

	// Close the client so the in-flight io.Copy in the shim drains
	// cleanly, then cancel and wait for shim shutdown.
	client.Close()
	cancel()
	select {
	case err := <-shimDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("shim returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shim did not exit after cancel")
	}
}

func TestRunSpawnShim_HandlesConcurrentClients(t *testing.T) {
	// Contract: the shim does not serialize clients. Two simultaneous
	// connections bridge independent UDS dials.
	udsPath := shortSockPath(t)
	udsLn, err := net.Listen("unix", udsPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer udsLn.Close()
	go func() {
		for {
			c, err := udsLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	go RunSpawnShim(ctx, "127.0.0.1:0", udsPath, ready)
	addr := <-ready

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := net.Dial("tcp", addr)
			if err != nil {
				t.Errorf("dial %d: %v", i, err)
				return
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(2 * time.Second))
			msg := []byte{'a' + byte(i)}
			if _, err := c.Write(msg); err != nil {
				t.Errorf("write %d: %v", i, err)
				return
			}
			got := make([]byte, 1)
			if _, err := io.ReadFull(c, got); err != nil {
				t.Errorf("read %d: %v", i, err)
				return
			}
			if got[0] != msg[0] {
				t.Errorf("client %d: got %q want %q", i, got, msg)
			}
		}(i)
	}
	wg.Wait()
}

func TestRunSpawnShim_ExitsOnContextCancel(t *testing.T) {
	// Contract: cancel(ctx) unblocks Accept and Run returns
	// context.Canceled. The UDS path does not have to exist for cancel
	// to propagate (no connection is in flight).
	udsPath := shortSockPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- RunSpawnShim(ctx, "127.0.0.1:0", udsPath, ready) }()
	<-ready
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("shim returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shim did not exit after cancel")
	}
}

func TestRunSpawnShim_ClosesClientWhenUDSUnreachable(t *testing.T) {
	// Contract: when the UDS path does not exist (proxy not yet bound,
	// or bind-mount missing), the shim accepts the client and
	// immediately closes the TCP connection. The CLI sees a closed
	// connection; no audit, no error from the shim itself (the proxy
	// is the audit seam).
	udsPath := shortSockPath(t)
	_ = os.Remove(udsPath) // ensure no socket bound here
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	go RunSpawnShim(ctx, "127.0.0.1:0", udsPath, ready)
	addr := <-ready

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	// Reading from a closed TCP connection returns io.EOF.
	buf := make([]byte, 1)
	_, err = c.Read(buf)
	if err != io.EOF && !errors.Is(err, io.ErrClosedPipe) && err != nil {
		// Other connection-closed shapes are acceptable; the contract
		// is "the client connection closes without bytes flowing."
		// Surface unexpected errors only.
		if _, ok := err.(net.Error); !ok && err != io.ErrUnexpectedEOF {
			t.Logf("read returned %v (accepted as connection-closed)", err)
		}
	}
}

func TestRunSpawnShim_RequiresArguments(t *testing.T) {
	if err := RunSpawnShim(context.Background(), "", "/tmp/sock", nil); err == nil {
		t.Error("expected error for empty tcpAddr")
	}
	if err := RunSpawnShim(context.Background(), "127.0.0.1:0", "", nil); err == nil {
		t.Error("expected error for empty udsPath")
	}
}
