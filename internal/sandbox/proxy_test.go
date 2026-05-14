package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/cstore"
)

// The contract tests below validate the proxy against ADR-0014's
// "Network confinement: daemon-mediated proxy" section. Each test
// states the property it enforces in a comment so a refactor that
// preserves the contract leaves the test green.

func TestSpawnProxy_TunnelsAllowedHost(t *testing.T) {
	// Contract: a CONNECT request to a host in [capabilities.network]
	// receives 200 Connection established and the bytes flow through.
	policy := policyForHosts(t, "allowed.test:443")

	// Fake upstream: echoes what it reads.
	upstream, upstreamReady := newEchoServer(t)
	defer upstream.Close()

	proxy := NewSpawnProxy(policy, nil, "github://aileron-test/x")
	proxy.SetDialer(func(ctx context.Context, network, addr string) (net.Conn, error) {
		// The proxy dials "allowed.test:443"; redirect to our fake echo server.
		if addr != "allowed.test:443" {
			return nil, fmt.Errorf("unexpected dial: %q", addr)
		}
		return net.Dial("tcp", upstream.Addr().String())
	})

	ln, addr := newLoopbackListener(t)
	go proxy.Serve(context.Background(), ln)
	defer proxy.Close()

	<-upstreamReady

	client, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer client.Close()
	client.SetDeadline(time.Now().Add(2 * time.Second))

	fmt.Fprintf(client, "CONNECT allowed.test:443 HTTP/1.1\r\nHost: allowed.test:443\r\n\r\n")
	br := bufio.NewReader(client)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Tunnel works: send a sentinel, read it back from echo upstream.
	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != "ping" {
		t.Errorf("echo = %q, want %q", got, "ping")
	}
}

func TestSpawnProxy_DeniesDisallowedHost(t *testing.T) {
	// Contract: a CONNECT request to a host outside
	// [capabilities.network] returns 403 and the audit row carries
	// `capability_denied`.
	policy := policyForHosts(t, "allowed.test:443")
	logBuf, logger := newCapturingLogger()
	proxy := NewSpawnProxy(policy, logger, "github://aileron-test/x")
	dialed := false
	proxy.SetDialer(func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = true
		return nil, errors.New("should not have dialed")
	})

	ln, addr := newLoopbackListener(t)
	go proxy.Serve(context.Background(), ln)
	defer proxy.Close()

	client, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	client.SetDeadline(time.Now().Add(2 * time.Second))

	fmt.Fprintf(client, "CONNECT denied.test:443 HTTP/1.1\r\nHost: denied.test:443\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != 403 {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if dialed {
		t.Error("proxy dialed upstream despite policy miss")
	}
	auditOut := logBuf.String()
	if !strings.Contains(auditOut, "spawn_network") {
		t.Errorf("audit log missing spawn_network: %s", auditOut)
	}
	if !strings.Contains(auditOut, "capability_denied") {
		t.Errorf("audit log missing capability_denied class: %s", auditOut)
	}
	if !strings.Contains(auditOut, "denied.test:443") {
		t.Errorf("audit log missing target host_port: %s", auditOut)
	}
}

func TestSpawnProxy_RejectsNonConnectMethod(t *testing.T) {
	// Contract: only CONNECT is honored. Plain GET (an agent sending
	// a regular HTTP request through the proxy) fails closed.
	policy := policyForHosts(t, "allowed.test:443")
	proxy := NewSpawnProxy(policy, nil, "x")
	ln, addr := newLoopbackListener(t)
	go proxy.Serve(context.Background(), ln)
	defer proxy.Close()

	client, _ := net.Dial("tcp", addr)
	defer client.Close()
	client.SetDeadline(time.Now().Add(2 * time.Second))
	fmt.Fprintf(client, "GET http://allowed.test/ HTTP/1.1\r\nHost: allowed.test\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != 405 {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestSpawnProxy_BadGatewayOnUpstreamFailure(t *testing.T) {
	// Contract: an allowlist-permitted host that cannot be reached
	// yields 502 with a structured audit row, not 403 (the connector
	// did not break the rules; the network broke).
	policy := policyForHosts(t, "allowed.test:443")
	logBuf, logger := newCapturingLogger()
	proxy := NewSpawnProxy(policy, logger, "x")
	proxy.SetDialer(func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, errors.New("simulated upstream failure")
	})

	ln, addr := newLoopbackListener(t)
	go proxy.Serve(context.Background(), ln)
	defer proxy.Close()

	client, _ := net.Dial("tcp", addr)
	defer client.Close()
	client.SetDeadline(time.Now().Add(2 * time.Second))
	fmt.Fprintf(client, "CONNECT allowed.test:443 HTTP/1.1\r\nHost: allowed.test:443\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != 502 {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if !strings.Contains(logBuf.String(), "upstream_unreachable") {
		t.Errorf("audit missing upstream_unreachable: %s", logBuf.String())
	}
}

func TestSpawnProxy_CloseStopsServing(t *testing.T) {
	// Contract: Close unblocks Serve and is idempotent.
	policy := policyForHosts(t)
	proxy := NewSpawnProxy(policy, nil, "x")
	ln, _ := newLoopbackListener(t)
	done := make(chan error, 1)
	go func() { done <- proxy.Serve(context.Background(), ln) }()
	if err := proxy.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := proxy.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v on Close", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after Close")
	}
}

// --- helpers ---

func policyForHosts(t *testing.T, hosts ...string) *HostPolicy {
	t.Helper()
	m := &cstore.Manifest{
		Capabilities: cstore.ManifestCapabilities{
			Network: &cstore.ManifestNetwork{Hosts: hosts},
		},
	}
	return NewHostPolicy(m)
}

func newLoopbackListener(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln, ln.Addr().String()
}

// newEchoServer returns a TCP server that echoes whatever bytes a
// connection writes to it. The returned channel is closed once the
// server is accepting connections.
func newEchoServer(t *testing.T) (net.Listener, <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo: %v", err)
	}
	ready := make(chan struct{})
	close(ready)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	return ln, ready
}

// newCapturingLogger returns an slog.Logger that writes JSON records
// to an in-memory buffer the test can inspect.
func newCapturingLogger() (*syncBuf, *slog.Logger) {
	buf := &syncBuf{}
	handler := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return buf, slog.New(handler)
}

// syncBuf is a thread-safe bytes.Buffer wrapper for the test logger.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
