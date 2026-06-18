package app

import (
	"bufio"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/audit"
	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
	"github.com/ALRubinger/aileron/internal/model"
)

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// websocketEchoUpstreamHandler is a minimal RFC 6455 server: it
// completes the opening handshake (validating the Connection/Upgrade
// headers survived the proxy) and echoes a single text frame back. A
// request that is NOT a websocket upgrade (e.g. headers stripped to a
// plain GET) is answered 405 Method Not Allowed — this is exactly the
// symptom the pre-fix proxy produced, so the regression test can assert
// on it.
func websocketEchoUpstreamHandler(observedUpgrade *string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if observedUpgrade != nil {
			*observedUpgrade = r.Header.Get("Upgrade")
		}
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
			!strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
			http.Error(w, "expected websocket upgrade", http.StatusMethodNotAllowed)
			return
		}
		key := r.Header.Get("Sec-WebSocket-Key")
		accept := websocketAcceptKey(key)

		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()

		_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\n"+
			"Connection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: "+accept+"\r\n\r\n")

		// Echo loop: read one client frame, write it back as a server
		// frame (unmasked).
		reader := bufio.NewReader(conn)
		payload, err := readWebSocketTextFrame(reader)
		if err != nil {
			return
		}
		_, _ = conn.Write(encodeWebSocketTextFrame(payload, false))
	}
}

func websocketAcceptKey(key string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, key+websocketGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// encodeWebSocketTextFrame encodes a single, final text frame. When
// masked is true the payload is masked per RFC 6455 (client-to-server
// frames must be masked).
func encodeWebSocketTextFrame(payload []byte, masked bool) []byte {
	var frame []byte
	frame = append(frame, 0x81) // FIN + text opcode
	length := len(payload)
	maskBit := byte(0)
	if masked {
		maskBit = 0x80
	}
	switch {
	case length < 126:
		frame = append(frame, maskBit|byte(length))
	case length < 1<<16:
		frame = append(frame, maskBit|126)
		var ext [2]byte
		binary.BigEndian.PutUint16(ext[:], uint16(length))
		frame = append(frame, ext[:]...)
	default:
		frame = append(frame, maskBit|127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(length))
		frame = append(frame, ext[:]...)
	}
	if masked {
		mask := []byte{0xAA, 0xBB, 0xCC, 0xDD}
		frame = append(frame, mask...)
		for i, b := range payload {
			frame = append(frame, b^mask[i%4])
		}
		return frame
	}
	return append(frame, payload...)
}

// readWebSocketTextFrame reads a single text frame, unmasking the
// payload when the mask bit is set.
func readWebSocketTextFrame(r *bufio.Reader) ([]byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	masked := header[1]&0x80 != 0
	length := int(header[1] & 0x7F)
	switch length {
	case 126:
		ext := make([]byte, 2)
		if _, err := io.ReadFull(r, ext); err != nil {
			return nil, err
		}
		length = int(binary.BigEndian.Uint16(ext))
	case 127:
		ext := make([]byte, 8)
		if _, err := io.ReadFull(r, ext); err != nil {
			return nil, err
		}
		length = int(binary.BigEndian.Uint64(ext))
	}
	var mask []byte
	if masked {
		mask = make([]byte, 4)
		if _, err := io.ReadFull(r, mask); err != nil {
			return nil, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return payload, nil
}

// dialThroughSandboxProxyTLS performs CONNECT host:443 through the
// proxy with Basic proxy auth, then completes the inner TLS handshake
// against the proxy's MITM leaf certificate. The returned conn is the
// in-container TLS connection: writing an HTTP request to it drives the
// decrypted-request handling path, exactly as the real in-container
// client does.
func dialThroughSandboxProxyTLS(t *testing.T, proxyURL *url.URL, roots *x509.CertPool, logicalHost string) net.Conn {
	t.Helper()
	raw, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	connectReq := "CONNECT " + logicalHost + ":443 HTTP/1.1\r\n" +
		"Host: " + logicalHost + ":443\r\n" +
		"Proxy-Authorization: " + basicProxyAuth("session-123", "daemon-token") + "\r\n\r\n"
	if _, err := io.WriteString(raw, connectReq); err != nil {
		raw.Close()
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		raw.Close()
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		raw.Close()
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}
	if br.Buffered() > 0 {
		t.Fatalf("unexpected buffered bytes after CONNECT response")
	}
	tlsConn := tls.Client(raw, &tls.Config{
		RootCAs:    roots,
		ServerName: logicalHost,
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err != nil {
		raw.Close()
		t.Fatalf("inner TLS handshake: %v", err)
	}
	t.Cleanup(func() { tlsConn.Close() })
	return tlsConn
}

// writeWebSocketUpgradeRequest writes a real RFC 6455 opening handshake
// request to conn for the given path.
func writeWebSocketUpgradeRequest(t *testing.T, conn net.Conn, logicalHost, path, key string) {
	t.Helper()
	req := fmt.Sprintf("GET %s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\n"+
		"Sec-WebSocket-Version: 13\r\n\r\n", path, logicalHost, key)
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatalf("write upgrade request: %v", err)
	}
}

func TestSandboxForwardProxy_WebSocketUpgradeTunnelsEchoRoundTrip(t *testing.T) {
	var observedUpgrade string
	setup := newPassthroughIntegrationSetup(t, websocketEchoUpstreamHandler(&observedUpgrade))

	conn := dialThroughSandboxProxyTLS(t, setup.proxyURL, setup.roots, setup.logicalHost)

	const key = "dGhlIHNhbXBsZSBub25jZQ=="
	writeWebSocketUpgradeRequest(t, conn, setup.logicalHost, "/backend-api/codex/responses", key)

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read handshake response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d, want 101", resp.StatusCode)
	}
	if got := resp.Header.Get("Upgrade"); !strings.EqualFold(got, "websocket") {
		t.Errorf("relayed Upgrade header = %q, want websocket", got)
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != websocketAcceptKey(key) {
		t.Errorf("Sec-WebSocket-Accept = %q, want %q", got, websocketAcceptKey(key))
	}

	// The upstream must have seen the Upgrade header preserved through the
	// proxy — the pre-fix bug stripped it.
	if !strings.EqualFold(observedUpgrade, "websocket") {
		t.Errorf("upstream observed Upgrade = %q, want websocket (header must survive the proxy)", observedUpgrade)
	}

	// Bidirectional tunnel: send a masked client frame, expect the echoed
	// payload back.
	const message = "hello over the tunnel"
	if _, err := conn.Write(encodeWebSocketTextFrame([]byte(message), true)); err != nil {
		t.Fatalf("write client frame: %v", err)
	}
	echoed, err := readWebSocketTextFrame(br)
	if err != nil {
		t.Fatalf("read echoed frame: %v", err)
	}
	if string(echoed) != message {
		t.Errorf("echoed payload = %q, want %q", string(echoed), message)
	}

	events, err := setup.auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1; events=%+v", len(events), events)
	}
	evt := events[0]
	if evt.EventType != model.EventTypeSandboxProxyUpgrade {
		t.Fatalf("event type = %q, want %q", evt.EventType, model.EventTypeSandboxProxyUpgrade)
	}
	if got := evt.Payload["aileron.proxy.upstream.host"]; got != setup.logicalHost {
		t.Errorf("upstream host = %v, want %s", got, setup.logicalHost)
	}
	if got := evt.Payload["aileron.proxy.upstream.path"]; got != "/backend-api/codex/responses" {
		t.Errorf("upstream path = %v, want /backend-api/codex/responses", got)
	}
	if got := evt.Payload["aileron.proxy.upstream.status"]; got != http.StatusSwitchingProtocols {
		t.Errorf("upstream status = %v, want 101", got)
	}
	if got := evt.Payload["aileron.session.id"]; got != "session-123" {
		t.Errorf("session id = %v, want session-123", got)
	}
	sandboxProxyUpgradeShape.validate(t, evt.Payload)
}

// TestSandboxForwardProxy_WebSocketUpgradeRegression is the regression
// guard for issue #1164: before the fix, the proxy stripped the
// Upgrade/Connection headers and forwarded a plain GET, so a WebSocket
// handshake arrived at the upstream as a non-upgrade request and was
// answered 405 Method Not Allowed (never a 101). This test asserts the
// pre-fix behavior is gone: the handshake yields 101, not a 4xx/5xx,
// and the Upgrade header reaches the upstream intact.
func TestSandboxForwardProxy_WebSocketUpgradeRegression(t *testing.T) {
	var observedUpgrade string
	setup := newPassthroughIntegrationSetup(t, websocketEchoUpstreamHandler(&observedUpgrade))

	conn := dialThroughSandboxProxyTLS(t, setup.proxyURL, setup.roots, setup.logicalHost)
	writeWebSocketUpgradeRequest(t, conn, setup.logicalHost, "/backend-api/codex/responses", "dGhlIHNhbXBsZSBub25jZQ==")

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read handshake response: %v", err)
	}
	if resp.StatusCode == http.StatusMethodNotAllowed {
		t.Fatalf("handshake returned 405 — pre-fix behavior (Upgrade header stripped, forwarded as plain GET)")
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d, want 101 (upgrade must complete)", resp.StatusCode)
	}
	if observedUpgrade == "" {
		t.Errorf("upstream observed no Upgrade header — the proxy stripped it (the #1164 bug)")
	}
}

// TestSandboxForwardProxy_WebSocketUpgradeRelaysNon101Verbatim covers
// the branch where the upstream declines the upgrade: the proxy relays
// the upstream's non-101 status and headers verbatim and records the
// upgrade audit event with that status, without opening a tunnel.
func TestSandboxForwardProxy_WebSocketUpgradeRelaysNon101Verbatim(t *testing.T) {
	setup := newPassthroughIntegrationSetup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Upstream rejects the handshake with 403 rather than 101.
		w.Header().Set("X-Upstream-Decline", "true")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("upgrade declined"))
	}))

	conn := dialThroughSandboxProxyTLS(t, setup.proxyURL, setup.roots, setup.logicalHost)
	writeWebSocketUpgradeRequest(t, conn, setup.logicalHost, "/ws", "dGhlIHNhbXBsZSBub25jZQ==")

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read handshake response: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (non-101 relayed verbatim)", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Upstream-Decline"); got != "true" {
		t.Errorf("X-Upstream-Decline = %q, want true (upstream header relayed)", got)
	}

	events, err := setup.auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].EventType != model.EventTypeSandboxProxyUpgrade {
		t.Fatalf("event type = %q, want %q", events[0].EventType, model.EventTypeSandboxProxyUpgrade)
	}
	if got := events[0].Payload["aileron.proxy.upstream.status"]; got != http.StatusForbidden {
		t.Errorf("upstream status = %v, want 403", got)
	}
}

// TestSandboxForwardProxy_WebSocketUpgradeUpstreamUnreachable covers the
// dial-failure branch: when the upstream cannot be reached the proxy
// returns 502 and records sandbox.proxy.rejected.
func TestSandboxForwardProxy_WebSocketUpgradeUpstreamUnreachable(t *testing.T) {
	stateDir := t.TempDir()
	caPEM := writeSandboxProxyTestCA(t, stateDir, "session-123")
	auditStore := audit.NewMemStore()
	srv := &apiServer{
		localDaemonToken:     "daemon-token",
		sandboxProxyStateDir: stateDir,
		auditRecorder:        audit.NewRecorder(auditStore, nil, func() string { return "audit-ws-unreachable" }),
		specLoader: func() ([]connectorspec.Spec, error) {
			return nil, nil
		},
		sandboxProxyClient: &http.Client{Transport: &http.Transport{
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				return nil, fmt.Errorf("dial refused")
			},
		}},
	}
	proxy := httptest.NewServer(srv.sandboxForwardProxyMiddleware(http.NotFoundHandler()))
	t.Cleanup(proxy.Close)

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to append CA cert")
	}
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}

	conn := dialThroughSandboxProxyTLS(t, proxyURL, roots, setupLogicalHostUnreachable)
	writeWebSocketUpgradeRequest(t, conn, setupLogicalHostUnreachable, "/ws", "dGhlIHNhbXBsZSBub25jZQ==")

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read handshake response: %v", err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (upstream unreachable)", resp.StatusCode)
	}

	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].EventType != model.EventTypeSandboxProxyRejected {
		t.Fatalf("event type = %q, want %q", events[0].EventType, model.EventTypeSandboxProxyRejected)
	}
	if got := events[0].Payload["aileron.proxy.reject_reason"]; got != "passthrough_upstream_unreachable" {
		t.Errorf("reject_reason = %v, want passthrough_upstream_unreachable", got)
	}
}

const setupLogicalHostUnreachable = "api.unreachable.test"

// TestSandboxForwardProxy_WebSocketUpgradeRefusesPrivateIPLiteral keeps
// the SSRF guard green on the upgrade path: a WebSocket handshake to a
// private/metadata IP literal must be refused at the passthrough
// boundary, never tunneled.
func TestSandboxForwardProxy_WebSocketUpgradeRefusesPrivateIPLiteral(t *testing.T) {
	stateDir := t.TempDir()
	caPEM := writeSandboxProxyTestCA(t, stateDir, "session-123")
	auditStore := audit.NewMemStore()
	srv := &apiServer{
		localDaemonToken:     "daemon-token",
		sandboxProxyStateDir: stateDir,
		auditRecorder:        audit.NewRecorder(auditStore, nil, func() string { return "audit-ws-ssrf" }),
		specLoader: func() ([]connectorspec.Spec, error) {
			return nil, nil
		},
		sandboxProxyClient: &http.Client{Transport: &http.Transport{
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				t.Fatalf("upgrade must not dial upstream for private IP literal target")
				return nil, nil
			},
		}},
	}
	proxy := httptest.NewServer(srv.sandboxForwardProxyMiddleware(http.NotFoundHandler()))
	t.Cleanup(proxy.Close)

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to append CA cert")
	}
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}

	conn := dialThroughSandboxProxyTLS(t, proxyURL, roots, "169.254.169.254")
	writeWebSocketUpgradeRequest(t, conn, "169.254.169.254", "/ws", "dGhlIHNhbXBsZSBub25jZQ==")

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read handshake response: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (private IP literal refused)", resp.StatusCode)
	}

	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].EventType != model.EventTypeSandboxProxyRejected {
		t.Fatalf("event type = %q, want %q", events[0].EventType, model.EventTypeSandboxProxyRejected)
	}
	if got := events[0].Payload["aileron.proxy.reject_reason"]; got != "passthrough_target_not_allowed" {
		t.Errorf("reject_reason = %v, want passthrough_target_not_allowed", got)
	}
}
