package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/internal/sandbox/discovery"
)

const sandboxForwardProxyRealm = "aileron sandbox proxy"
const sandboxForwardProxyMaxRequestBytes = 1 << 20

var errSandboxForwardProxyAmbiguousOperation = errors.New("sandbox proxy decrypted request matched multiple connector operations")

type sandboxProxyAuth struct {
	SessionID string
	Token     string
}

func (s *apiServer) sandboxForwardProxyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isSandboxForwardProxyRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		s.HandleSandboxForwardProxy(w, r)
	})
}

func isSandboxForwardProxyRequest(r *http.Request) bool {
	return r.Method == http.MethodConnect || strings.TrimSpace(r.Header.Get("Proxy-Authorization")) != ""
}

func (s *apiServer) HandleSandboxForwardProxy(w http.ResponseWriter, r *http.Request) {
	auth, ok := s.authenticateSandboxForwardProxy(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodConnect {
		s.handleSandboxForwardProxyConnect(w, r, auth)
		return
	}
	upstream := sandboxForwardProxyAbsoluteFormUpstream(r)
	s.recordSandboxProxyProtocolRejected(r, sandboxProxySourceTransparentConnectTLS, r.Method, upstream, "non_connect_proxy_request_unsupported")
	w.Header().Set("X-Aileron-Session-Id", auth.SessionID)
	http.Error(w, "sandbox HTTPS forward proxy requires CONNECT for HTTPS interception; absolute-form proxy requests without CONNECT are not supported", http.StatusForbidden)
}

// sandboxForwardProxyAbsoluteFormUpstream returns a best-effort upstream
// URL for audit emission on the non-CONNECT proxy path. Absolute-form
// proxy requests carry the full URL on r.URL; relative-form requests
// fall back to https://r.Host/r.URL.Path so the audit always has a
// host and path to report.
func sandboxForwardProxyAbsoluteFormUpstream(r *http.Request) *url.URL {
	if r.URL != nil && r.URL.IsAbs() {
		return r.URL
	}
	host := strings.TrimSpace(r.Host)
	if host == "" && r.URL != nil {
		host = r.URL.Host
	}
	path := "/"
	if r.URL != nil && r.URL.Path != "" {
		path = r.URL.Path
	}
	upstream, _ := parseSandboxProxyUpstreamURL("https://" + host + path)
	if upstream == nil {
		upstream = &url.URL{Scheme: "https", Host: host, Path: path}
	}
	return upstream
}

type sandboxForwardProxyConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *sandboxForwardProxyConn) Read(p []byte) (int, error) {
	if c.reader != nil && c.reader.Buffered() > 0 {
		return c.reader.Read(p)
	}
	return c.Conn.Read(p)
}

func (s *apiServer) handleSandboxForwardProxyConnect(w http.ResponseWriter, r *http.Request, auth sandboxProxyAuth) {
	targetHost, err := sandboxForwardProxyConnectHost(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cert, err := s.sandboxForwardProxyCertificate(auth.SessionID, targetHost)
	if err != nil {
		upstream, _ := parseSandboxProxyUpstreamURL("https://" + targetHost + "/")
		if upstream == nil {
			upstream = &url.URL{Scheme: "https", Host: targetHost, Path: "/"}
		}
		s.recordSandboxProxyProtocolRejected(r, sandboxProxySourceTransparentConnectTLS, r.Method, upstream, "session_ca_unavailable")
		w.Header().Set("X-Aileron-Session-Id", auth.SessionID)
		http.Error(w, "sandbox proxy session CA is unavailable for this session; the launcher's session state directory may be missing or corrupted (see ADR-0019)", http.StatusInternalServerError)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "sandbox HTTPS forward proxy requires connection hijacking", http.StatusInternalServerError)
		return
	}
	clientConn, rw, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer clientConn.Close()

	if _, err := fmt.Fprintf(clientConn, "HTTP/1.1 200 Connection Established\r\nX-Aileron-Session-Id: %s\r\n\r\n", auth.SessionID); err != nil {
		return
	}

	tlsConn := tls.Server(&sandboxForwardProxyConn{Conn: clientConn, reader: rw.Reader}, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	if err := tlsConn.HandshakeContext(r.Context()); err != nil {
		return
	}
	defer tlsConn.Close()

	decrypted, err := http.ReadRequest(bufio.NewReader(tlsConn))
	if err != nil {
		_, _ = tlsConn.Write([]byte("HTTP/1.1 400 Bad Request\r\nConnection: close\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: 45\r\n\r\nsandbox proxy decrypted request is invalid\n"))
		return
	}
	defer decrypted.Body.Close()
	decrypted = decrypted.WithContext(r.Context())
	decrypted.Header.Set("X-Aileron-Session-Id", auth.SessionID)
	s.handleSandboxForwardProxyDecrypted(tlsConn, decrypted, auth, targetHost)
}

func (s *apiServer) handleSandboxForwardProxyDecrypted(conn net.Conn, decrypted *http.Request, auth sandboxProxyAuth, targetHost string) {
	upstream, err := sandboxForwardProxyUpstreamURL(targetHost, decrypted)
	if err != nil {
		writeSandboxForwardProxyError(conn, http.StatusBadRequest, auth.SessionID, targetHost, "sandbox proxy decrypted request target is invalid")
		return
	}
	// A WebSocket (or other HTTP Upgrade) handshake cannot be served by
	// the buffered client.Do path: the upstream answers 101 and opens a
	// bidirectional byte tunnel, not a single buffered response. These
	// requests are forwarded through the passthrough boundary with the
	// Upgrade/Connection/Sec-WebSocket-* headers preserved. No connector
	// spec match is attempted because credential injection is not
	// meaningful for a raw byte tunnel (ADR-0019 passthrough posture).
	if sandboxForwardProxyIsWebSocketUpgrade(decrypted) {
		s.upgradeSandboxForwardProxy(conn, decrypted, auth, targetHost, upstream)
		return
	}
	match, ok, err := s.matchSandboxForwardProxyOperation(decrypted.Method, upstream)
	if err != nil {
		if errors.Is(err, errSandboxForwardProxyAmbiguousOperation) {
			// Precedence: connector-spec -> host-binding -> passthrough.
			// An ambiguous connector-spec match yields no unique spec, so
			// the host-binding table is consulted before passthrough,
			// exactly as on the no-match branch below.
			if s.routeSandboxForwardProxyHostBinding(conn, decrypted, auth, targetHost, upstream) {
				return
			}
			s.passthroughSandboxForwardProxy(conn, decrypted, auth, targetHost, upstream)
			return
		}
		s.recordSandboxProxyProtocolRejected(decrypted, sandboxProxySourceTransparentConnectTLS, decrypted.Method, upstream, sandboxForwardProxyMatchErrorReason(err))
		writeSandboxForwardProxyError(conn, http.StatusInternalServerError, auth.SessionID, targetHost, err.Error())
		return
	}
	if !ok {
		// Precedence: a unique connector-spec match wins (handled below);
		// otherwise the host-binding table is consulted before falling to
		// passthrough.
		if s.routeSandboxForwardProxyHostBinding(conn, decrypted, auth, targetHost, upstream) {
			return
		}
		s.passthroughSandboxForwardProxy(conn, decrypted, auth, targetHost, upstream)
		return
	}
	body, contentType, err := readSandboxForwardProxyRequestBody(decrypted)
	if err != nil {
		s.recordSandboxProxyRejected(decrypted, sandboxProxySourceTransparentConnectTLS, match.connectorFQN, match.toolName, decrypted.Method, upstream, match.operation, sandboxForwardProxyBodyRejectReason(err))
		writeSandboxForwardProxyError(conn, http.StatusRequestEntityTooLarge, auth.SessionID, targetHost, err.Error())
		return
	}

	captured := newSandboxForwardProxyCapture()
	result, ok := s.executeSandboxProxyRequest(captured, decrypted, sandboxProxySourceTransparentConnectTLS, match.connectorFQN, match.toolName, decrypted.Method, upstream, match.operation, body, contentType)
	if !ok {
		writeSandboxForwardProxyCaptured(conn, auth.SessionID, targetHost, captured)
		return
	}
	writeSandboxForwardProxyResponse(conn, auth.SessionID, targetHost, result.UpstreamStatus, derefString(result.ContentType), result.BodyBase64)
}

// routeSandboxForwardProxyHostBinding consults the user-level
// host->credential binding table (#1193) for the decrypted request's
// target host. It is called only when no unique connector spec matched,
// so the precedence is connector-spec -> host-binding -> passthrough.
//
// On a binding match it resolves the bound credential daemon-side
// (through the vault), injects it onto a fresh upstream request per the
// binding's scheme, re-issues the request, and streams the response
// back to the in-container client. The credential bytes never reach the
// container: only the upstream status, headers, and body are written
// back. On a resolve/inject failure the request fails closed with an
// error response carrying no secret bytes, and a rejection is recorded;
// passthrough is NOT taken for a bound host whose credential is
// unavailable. It returns handled=false (without writing anything) only
// when no binding matched, so the caller falls through to passthrough.
//
// The private-IP-literal refusal that guards the passthrough path is
// applied here too: a bound host that is a private IP literal is
// refused before any upstream dial, closing the same SSRF vector.
func (s *apiServer) routeSandboxForwardProxyHostBinding(conn net.Conn, decrypted *http.Request, auth sandboxProxyAuth, targetHost string, upstream *url.URL) bool {
	hostForMatch := targetHost
	if h, _, err := net.SplitHostPort(targetHost); err == nil {
		hostForMatch = h
	}
	hb, ok := s.hostBindings.Match(hostForMatch)
	if !ok {
		return false
	}

	if sandboxForwardProxyTargetIsPrivateIPLiteral(targetHost) {
		s.recordSandboxProxyProtocolRejected(decrypted, sandboxProxySourceTransparentConnectTLS, decrypted.Method, upstream, "passthrough_target_not_allowed")
		writeSandboxForwardProxyError(conn, http.StatusForbidden, auth.SessionID, targetHost, "sandbox proxy passthrough refuses private, loopback, or link-local IP literals")
		return true
	}

	// Per-step trust-contract gate (#1735). A bound host that carries a
	// declared trust contract (a non-empty AllowedHosts and/or Effect) is
	// gated here, BEFORE any credential is resolved or injected, so the gate
	// covers both the inject and the sentinel-swap-foreign-token branches
	// below (all egress on a bound host). A binding with no declared
	// contract passes unconstrained, exactly as before. A gate failure is a
	// denial: it writes a 403 and returns handled — it never falls through
	// to passthrough.
	if reason, ok := enforceHostBindingTrust(hb, upstream, decrypted.Method); !ok {
		s.recordSandboxProxyTrustDenied(decrypted, sandboxProxySourceTransparentConnectTLS, hb, upstream, reason)
		writeSandboxForwardProxyError(conn, http.StatusForbidden, auth.SessionID, targetHost, "sandbox proxy denied egress: the request violates the host binding's trust contract")
		return true
	}

	body, contentType, err := readSandboxForwardProxyRequestBody(decrypted)
	if err != nil {
		s.recordSandboxProxyProtocolRejected(decrypted, sandboxProxySourceTransparentConnectTLS, decrypted.Method, upstream, sandboxForwardProxyBodyRejectReason(err))
		writeSandboxForwardProxyError(conn, http.StatusRequestEntityTooLarge, auth.SessionID, targetHost, err.Error())
		return true
	}

	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	upstreamReq, err := http.NewRequestWithContext(decrypted.Context(), decrypted.Method, upstream.String(), reqBody)
	if err != nil {
		s.recordSandboxProxyProtocolRejected(decrypted, sandboxProxySourceTransparentConnectTLS, decrypted.Method, upstream, "binding_upstream_request_invalid")
		writeSandboxForwardProxyError(conn, http.StatusBadGateway, auth.SessionID, targetHost, "sandbox proxy host-binding upstream request is invalid")
		return true
	}
	upstreamReq.ContentLength = int64(len(body))
	copyPassthroughRequestHeaders(upstreamReq.Header, decrypted.Header)
	if contentType != "" {
		upstreamReq.Header.Set("Content-Type", contentType)
	}

	// Sentinel-swap gate. For a sentinel-swap binding the proxy seals only
	// tokens it itself planted: the recognized sentinel is stripped here and
	// replaced by the real credential below, while a foreign token is
	// forwarded unchanged with no real secret injected. An inject binding
	// always injects.
	switch decideSentinelSwap(upstreamReq, hb) {
	case sentinelSwapPassthroughForeign:
		// The agent supplied its own token on a sentinel-swap host. Do not
		// swap: dial upstream with the foreign carrier intact and no real
		// credential injected. The sentinel/secret never enter this path.
		resp, err := s.dialSandboxForwardProxyHostBinding(conn, decrypted, auth, targetHost, upstream, upstreamReq)
		if err != nil {
			return true
		}
		defer resp.body.Close()
		s.recordSandboxProxyForeignTokenNotSwapped(decrypted, sandboxProxySourceTransparentConnectTLS, hb.HostPattern, hb.Scheme, upstream, resp.statusCode)
		writeSandboxForwardProxyPassthroughResponse(conn, auth.SessionID, targetHost, resp.resp, resp.bodyBytes)
		return true
	case sentinelSwapInject:
		// fall through to credential injection below.
	}

	if injected, reason := s.injectSandboxProxyHostBindingCredential(decrypted.Context(), upstreamReq, hb); !injected {
		status := http.StatusInternalServerError
		if reason == hostBindingRejectUnsupportedScheme {
			status = http.StatusForbidden
		}
		s.recordSandboxProxyProtocolRejected(decrypted, sandboxProxySourceTransparentConnectTLS, decrypted.Method, upstream, reason)
		writeSandboxForwardProxyError(conn, status, auth.SessionID, targetHost, "sandbox proxy host-binding credential is unavailable")
		return true
	}

	dialed, err := s.dialSandboxForwardProxyHostBinding(conn, decrypted, auth, targetHost, upstream, upstreamReq)
	if err != nil {
		return true
	}
	defer dialed.body.Close()

	s.recordSandboxProxyBindingInjected(decrypted, sandboxProxySourceTransparentConnectTLS, hb, upstream, dialed.statusCode)
	writeSandboxForwardProxyPassthroughResponse(conn, auth.SessionID, targetHost, dialed.resp, dialed.bodyBytes)
	return true
}

// sandboxForwardProxyHostBindingResponse carries an upstream response
// dialed on the host-binding path back to the caller so the caller can
// record its own audit decision (binding_injected vs.
// foreign_token_not_swapped) before writing the response. body is the
// response body the caller must Close; bodyBytes is the size-capped,
// already-read body to write back.
type sandboxForwardProxyHostBindingResponse struct {
	resp       *http.Response
	body       io.Closer
	bodyBytes  []byte
	statusCode int
}

// dialSandboxForwardProxyHostBinding dials the prepared upstream request
// on the host-binding path, reading and size-capping the response. On any
// failure it records the matching protocol-rejection audit event, writes
// the error envelope to conn, and returns a non-nil error so the caller
// returns handled. On success the caller owns recording the audit
// decision and closing the returned body.
func (s *apiServer) dialSandboxForwardProxyHostBinding(conn net.Conn, decrypted *http.Request, auth sandboxProxyAuth, targetHost string, upstream *url.URL, upstreamReq *http.Request) (sandboxForwardProxyHostBindingResponse, error) {
	client := sandboxProxyHTTPClient(s.sandboxProxyClient)
	resp, err := client.Do(upstreamReq)
	if err != nil {
		s.recordSandboxProxyProtocolRejected(decrypted, sandboxProxySourceTransparentConnectTLS, decrypted.Method, upstream, "binding_upstream_unreachable")
		writeSandboxForwardProxyError(conn, http.StatusBadGateway, auth.SessionID, targetHost, "sandbox proxy host-binding upstream unreachable")
		return sandboxForwardProxyHostBindingResponse{}, err
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, sandboxProxyMaxResponseBytes+1))
	if err != nil {
		resp.Body.Close()
		s.recordSandboxProxyProtocolRejected(decrypted, sandboxProxySourceTransparentConnectTLS, decrypted.Method, upstream, "binding_upstream_read_failed")
		writeSandboxForwardProxyError(conn, http.StatusBadGateway, auth.SessionID, targetHost, "sandbox proxy host-binding upstream response read failed")
		return sandboxForwardProxyHostBindingResponse{}, err
	}
	if len(respBody) > sandboxProxyMaxResponseBytes {
		resp.Body.Close()
		s.recordSandboxProxyProtocolRejected(decrypted, sandboxProxySourceTransparentConnectTLS, decrypted.Method, upstream, "binding_upstream_response_too_large")
		writeSandboxForwardProxyError(conn, http.StatusBadGateway, auth.SessionID, targetHost, "sandbox proxy host-binding upstream response exceeded size limit")
		return sandboxForwardProxyHostBindingResponse{}, fmt.Errorf("sandbox proxy host-binding upstream response exceeded size limit")
	}

	return sandboxForwardProxyHostBindingResponse{
		resp:       resp,
		body:       resp.Body,
		bodyBytes:  respBody,
		statusCode: resp.StatusCode,
	}, nil
}

// passthroughSandboxForwardProxy forwards a decrypted CONNECT/TLS
// request to its upstream unmodified and streams the response back
// through the established TLS connection. Used when the request did
// not uniquely match an installed connector operation under the
// credential-injection-only model (ADR-0019). No credential is
// injected by the proxy. The audit event is sandbox.proxy.passthrough.
//
// The in-container client's own request body is forwarded as-is with
// no size cap. Passthrough is byte-for-byte; the daemon streams the
// body straight to the upstream. Resource budgets (concurrent
// connections, CPU, network) are operator-managed at the container
// runtime layer, not the proxy. The matched/credentialed path
// separately caps the buffered body at sandboxForwardProxyMaxRequestBytes
// because that path re-issues the request after credential injection.
//
// IP-literal targets in loopback, link-local, RFC1918, IPv6 ULA, or
// carrier-grade NAT space are refused at the passthrough boundary.
// Under Model A, an unfiltered passthrough would let an in-container
// client coerce the daemon into dialing 169.254.169.254 (cloud
// metadata) or other host-local addresses, exfiltrating data the
// agent should never see. Hostnames that resolve to private IPs are
// not blocked at this layer (DNS-rebinding is out of scope for this
// control; container-level network hardening is the deeper defense).
func (s *apiServer) passthroughSandboxForwardProxy(conn net.Conn, decrypted *http.Request, auth sandboxProxyAuth, targetHost string, upstream *url.URL) {
	if sandboxForwardProxyTargetIsPrivateIPLiteral(targetHost) {
		s.recordSandboxProxyProtocolRejected(decrypted, sandboxProxySourceTransparentConnectTLS, decrypted.Method, upstream, "passthrough_target_not_allowed")
		writeSandboxForwardProxyError(conn, http.StatusForbidden, auth.SessionID, targetHost, "sandbox proxy passthrough refuses private, loopback, or link-local IP literals")
		return
	}
	var reqBody io.Reader
	if decrypted.Body != nil {
		reqBody = decrypted.Body
	}
	upstreamReq, err := http.NewRequestWithContext(decrypted.Context(), decrypted.Method, upstream.String(), reqBody)
	if err != nil {
		s.recordSandboxProxyProtocolRejected(decrypted, sandboxProxySourceTransparentConnectTLS, decrypted.Method, upstream, "passthrough_upstream_request_invalid")
		writeSandboxForwardProxyError(conn, http.StatusBadGateway, auth.SessionID, targetHost, "sandbox proxy passthrough upstream request is invalid")
		return
	}
	upstreamReq.ContentLength = decrypted.ContentLength
	copyPassthroughRequestHeaders(upstreamReq.Header, decrypted.Header)

	client := sandboxProxyHTTPClient(s.sandboxProxyClient)
	resp, err := client.Do(upstreamReq)
	if err != nil {
		s.recordSandboxProxyProtocolRejected(decrypted, sandboxProxySourceTransparentConnectTLS, decrypted.Method, upstream, "passthrough_upstream_unreachable")
		writeSandboxForwardProxyError(conn, http.StatusBadGateway, auth.SessionID, targetHost, "sandbox proxy passthrough upstream unreachable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, sandboxProxyMaxResponseBytes+1))
	if err != nil {
		s.recordSandboxProxyProtocolRejected(decrypted, sandboxProxySourceTransparentConnectTLS, decrypted.Method, upstream, "passthrough_upstream_read_failed")
		writeSandboxForwardProxyError(conn, http.StatusBadGateway, auth.SessionID, targetHost, "sandbox proxy passthrough upstream response read failed")
		return
	}
	if len(body) > sandboxProxyMaxResponseBytes {
		s.recordSandboxProxyProtocolRejected(decrypted, sandboxProxySourceTransparentConnectTLS, decrypted.Method, upstream, "passthrough_upstream_response_too_large")
		writeSandboxForwardProxyError(conn, http.StatusBadGateway, auth.SessionID, targetHost, "sandbox proxy passthrough upstream response exceeded size limit")
		return
	}

	s.recordSandboxProxyPassthrough(decrypted, sandboxProxySourceTransparentConnectTLS, decrypted.Method, upstream, resp.StatusCode)
	writeSandboxForwardProxyPassthroughResponse(conn, auth.SessionID, targetHost, resp, body)
}

// sandboxForwardProxyIsWebSocketUpgrade reports whether the decrypted
// request is a WebSocket handshake: method GET, a Connection header
// whose token list contains "upgrade", and an Upgrade header naming
// "websocket". The token checks are case-insensitive per RFC 6455 and
// RFC 7230 (Connection is a comma-separated token list).
func sandboxForwardProxyIsWebSocketUpgrade(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return false
	}
	return headerListContainsToken(r.Header.Values("Connection"), "upgrade")
}

// headerListContainsToken reports whether any of the comma-separated
// header values contains the given token (case-insensitive).
func headerListContainsToken(values []string, token string) bool {
	for _, v := range values {
		for _, field := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(field), token) {
				return true
			}
		}
	}
	return false
}

// upgradeSandboxForwardProxy relays a WebSocket (or other HTTP Upgrade)
// handshake through the passthrough boundary and, on a successful 101,
// opens a bidirectional byte tunnel between the in-container client and
// the upstream. The Upgrade/Connection/Sec-WebSocket-* headers are
// preserved verbatim (unlike the buffered passthrough path, which
// strips hop-by-hop headers). The upstream is dialed over TLS using the
// configured sandbox proxy transport's dialer and TLS config. No
// credential is injected (ADR-0019 passthrough posture).
//
// The tunnel is byte-for-byte with no response-size cap, consistent
// with the passthrough model's no-cap rationale: resource budgets are
// operator-managed at the container runtime layer, not the proxy. The
// private-IP-literal refusal guard applies here exactly as it does on
// the buffered passthrough path.
func (s *apiServer) upgradeSandboxForwardProxy(conn net.Conn, decrypted *http.Request, auth sandboxProxyAuth, targetHost string, upstream *url.URL) {
	if sandboxForwardProxyTargetIsPrivateIPLiteral(targetHost) {
		s.recordSandboxProxyProtocolRejected(decrypted, sandboxProxySourceTransparentConnectTLS, decrypted.Method, upstream, "passthrough_target_not_allowed")
		writeSandboxForwardProxyError(conn, http.StatusForbidden, auth.SessionID, targetHost, "sandbox proxy passthrough refuses private, loopback, or link-local IP literals")
		return
	}

	upstreamConn, err := s.dialSandboxForwardProxyUpstreamTLS(decrypted.Context(), targetHost)
	if err != nil {
		s.recordSandboxProxyProtocolRejected(decrypted, sandboxProxySourceTransparentConnectTLS, decrypted.Method, upstream, "passthrough_upstream_unreachable")
		writeSandboxForwardProxyError(conn, http.StatusBadGateway, auth.SessionID, targetHost, "sandbox proxy upgrade upstream unreachable")
		return
	}
	defer upstreamConn.Close()

	upstreamReq := sandboxForwardProxyUpgradeRequest(decrypted, targetHost, upstream)
	if err := upstreamReq.Write(upstreamConn); err != nil {
		s.recordSandboxProxyProtocolRejected(decrypted, sandboxProxySourceTransparentConnectTLS, decrypted.Method, upstream, "passthrough_upstream_unreachable")
		writeSandboxForwardProxyError(conn, http.StatusBadGateway, auth.SessionID, targetHost, "sandbox proxy upgrade upstream write failed")
		return
	}

	upstreamReader := bufio.NewReader(upstreamConn)
	resp, err := http.ReadResponse(upstreamReader, upstreamReq)
	if err != nil {
		s.recordSandboxProxyProtocolRejected(decrypted, sandboxProxySourceTransparentConnectTLS, decrypted.Method, upstream, "passthrough_upstream_read_failed")
		writeSandboxForwardProxyError(conn, http.StatusBadGateway, auth.SessionID, targetHost, "sandbox proxy upgrade upstream response read failed")
		return
	}

	// Relay the upstream's handshake response (status line + headers)
	// verbatim back to the in-container client. The X-Aileron headers are
	// injected so the client can correlate the tunnel with its session.
	if err := writeSandboxForwardProxyUpgradeResponse(conn, auth.SessionID, targetHost, resp); err != nil {
		resp.Body.Close()
		return
	}

	s.recordSandboxProxyUpgrade(decrypted, sandboxProxySourceTransparentConnectTLS, decrypted.Method, upstream, resp.StatusCode)

	if resp.StatusCode != http.StatusSwitchingProtocols {
		// Upstream declined the upgrade. The status and headers have been
		// relayed; drain and close without opening a tunnel.
		resp.Body.Close()
		return
	}
	resp.Body.Close()

	sandboxForwardProxyTunnel(conn, upstreamConn, upstreamReader)
}

// dialSandboxForwardProxyUpstreamTLS dials the upstream over TLS,
// reusing the configured sandbox proxy transport's DialContext and TLS
// config when present so tests (and any custom transport wiring) take
// effect for the upgrade path exactly as they do for buffered
// passthrough.
func (s *apiServer) dialSandboxForwardProxyUpstreamTLS(ctx context.Context, targetHost string) (net.Conn, error) {
	addr := targetHost
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(targetHost, "443")
	}
	serverName := targetHost
	if h, _, err := net.SplitHostPort(targetHost); err == nil {
		serverName = h
	}

	dialContext := (&net.Dialer{Timeout: 30 * time.Second}).DialContext
	var tlsConfig *tls.Config
	if s.sandboxProxyClient != nil {
		if tr, ok := s.sandboxProxyClient.Transport.(*http.Transport); ok && tr != nil {
			if tr.DialContext != nil {
				dialContext = tr.DialContext
			}
			if tr.TLSClientConfig != nil {
				tlsConfig = tr.TLSClientConfig.Clone()
			}
		}
	}
	if tlsConfig == nil {
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	if tlsConfig.ServerName == "" {
		tlsConfig.ServerName = serverName
	}

	rawConn, err := dialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	tlsConn := tls.Client(rawConn, tlsConfig)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, err
	}
	return tlsConn, nil
}

// sandboxForwardProxyUpgradeRequest builds the upstream-facing upgrade
// request, copying every client header verbatim (including the
// Upgrade, Connection, and Sec-WebSocket-* headers that the buffered
// passthrough path strips as hop-by-hop). The proxy-specific session
// header is removed so it never leaks to the upstream.
func sandboxForwardProxyUpgradeRequest(decrypted *http.Request, targetHost string, upstream *url.URL) *http.Request {
	req := &http.Request{
		Method:     http.MethodGet,
		URL:        &url.URL{Path: upstream.Path, RawQuery: upstream.RawQuery},
		Host:       targetHost,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header, len(decrypted.Header)),
	}
	for k, vs := range decrypted.Header {
		if http.CanonicalHeaderKey(k) == "X-Aileron-Session-Id" {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return req
}

// writeSandboxForwardProxyUpgradeResponse relays the upstream handshake
// response (status line and headers) verbatim to the in-container
// client, appending the X-Aileron correlation headers.
func writeSandboxForwardProxyUpgradeResponse(conn net.Conn, sessionID, targetHost string, resp *http.Response) error {
	sessionID = sandboxForwardProxyHeaderValue(sessionID)
	targetHost = sandboxForwardProxyHeaderValue(targetHost)

	var sb strings.Builder
	statusText := resp.Status
	if i := strings.IndexByte(statusText, ' '); i >= 0 {
		statusText = strings.TrimSpace(statusText[i+1:])
	}
	if statusText == "" {
		statusText = http.StatusText(resp.StatusCode)
	}
	fmt.Fprintf(&sb, "HTTP/1.1 %d %s\r\n", resp.StatusCode, statusText)
	for _, k := range sortedHeaderKeys(resp.Header) {
		for _, v := range resp.Header.Values(k) {
			fmt.Fprintf(&sb, "%s: %s\r\n", k, sandboxForwardProxyHeaderValue(v))
		}
	}
	fmt.Fprintf(&sb, "X-Aileron-Session-Id: %s\r\nX-Aileron-Proxy-Upstream-Host: %s\r\n\r\n", sessionID, targetHost)

	_, err := io.WriteString(conn, sb.String())
	return err
}

// sandboxForwardProxyTunnel copies bytes bidirectionally between the
// in-container client connection and the upstream connection until
// either side closes. Any bytes already buffered in upstreamReader
// (read past the handshake response) are flushed to the client first.
func sandboxForwardProxyTunnel(clientConn, upstreamConn net.Conn, upstreamReader *bufio.Reader) {
	done := make(chan struct{}, 2)
	go func() {
		if n := upstreamReader.Buffered(); n > 0 {
			if buffered, err := upstreamReader.Peek(n); err == nil {
				_, _ = clientConn.Write(buffered)
				_, _ = upstreamReader.Discard(n)
			}
		}
		_, _ = io.Copy(clientConn, upstreamReader)
		closeWriteOrConn(clientConn)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(upstreamConn, clientConn)
		closeWriteOrConn(upstreamConn)
		done <- struct{}{}
	}()
	<-done
	<-done
}

// closeWriteOrConn half-closes the write side of conn when supported
// (TLS and TCP conns expose CloseWrite), signaling EOF to the peer
// without tearing down the read direction. Falls back to a full close.
func closeWriteOrConn(conn net.Conn) {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := conn.(closeWriter); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = conn.Close()
}

// sandboxForwardProxyTargetIsPrivateIPLiteral reports whether the
// CONNECT target host is an IP literal in a private, loopback,
// link-local, IPv6 ULA, or carrier-grade NAT range. These targets are
// refused at the passthrough boundary because the daemon runs on the
// host network namespace, so passthrough to such addresses lets an
// in-container client reach the host VM's cloud metadata endpoint
// (e.g. 169.254.169.254) or arbitrary internal infrastructure, both
// of which are credential-leak risks. Hostnames that resolve to
// private IPs are not blocked here; DNS-rebinding protection lives
// at a different layer.
func sandboxForwardProxyTargetIsPrivateIPLiteral(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsPrivate() {
		return true
	}
	// Carrier-grade NAT (RFC 6598) is not classified by IsPrivate.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return true
	}
	return false
}

func copyPassthroughRequestHeaders(dst, src http.Header) {
	for k, vs := range src {
		if isSandboxForwardProxyHopByHopRequestHeader(k) {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func isSandboxForwardProxyHopByHopRequestHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade",
		"X-Aileron-Session-Id",
		"Host":
		return true
	}
	return false
}

func isSandboxForwardProxyHopByHopResponseHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade",
		"Content-Length":
		return true
	}
	return false
}

func writeSandboxForwardProxyPassthroughResponse(conn io.Writer, sessionID, targetHost string, resp *http.Response, body []byte) {
	sessionID = sandboxForwardProxyHeaderValue(sessionID)
	targetHost = sandboxForwardProxyHeaderValue(targetHost)

	var sb strings.Builder
	statusText := resp.Status
	if i := strings.IndexByte(statusText, ' '); i >= 0 {
		statusText = strings.TrimSpace(statusText[i+1:])
	}
	if statusText == "" {
		statusText = http.StatusText(resp.StatusCode)
	}
	fmt.Fprintf(&sb, "HTTP/1.1 %d %s\r\n", resp.StatusCode, statusText)

	for _, k := range sortedHeaderKeys(resp.Header) {
		if isSandboxForwardProxyHopByHopResponseHeader(k) {
			continue
		}
		for _, v := range resp.Header.Values(k) {
			fmt.Fprintf(&sb, "%s: %s\r\n", k, sandboxForwardProxyHeaderValue(v))
		}
	}

	fmt.Fprintf(&sb, "Connection: close\r\nX-Aileron-Session-Id: %s\r\nX-Aileron-Proxy-Upstream-Host: %s\r\nContent-Length: %d\r\n\r\n", sessionID, targetHost, len(body))

	_, _ = io.WriteString(conn, sb.String())
	if len(body) > 0 {
		_, _ = conn.Write(body)
	}
}

func sortedHeaderKeys(h http.Header) []string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

type sandboxForwardProxyOperationMatch struct {
	connectorFQN string
	toolName     string
	operation    discovery.SpecOperationHelp
}

func (s *apiServer) matchSandboxForwardProxyOperation(method string, upstream *url.URL) (sandboxForwardProxyOperationMatch, bool, error) {
	specs, err := s.loadConnectorOperationSpecs()
	if err != nil {
		return sandboxForwardProxyOperationMatch{}, false, fmt.Errorf("connector specs unavailable")
	}
	tools, err := discovery.SpecConnectorTools(specs)
	if err != nil {
		return sandboxForwardProxyOperationMatch{}, false, fmt.Errorf("connector specs invalid")
	}
	var match sandboxForwardProxyOperationMatch
	for _, tool := range tools {
		for _, operation := range tool.Operations {
			if !sandboxProxyRequestMatchesOperation(method, upstream, operation) {
				continue
			}
			if match.operation.Name != "" {
				return sandboxForwardProxyOperationMatch{}, false, errSandboxForwardProxyAmbiguousOperation
			}
			match = sandboxForwardProxyOperationMatch{
				connectorFQN: tool.FQN,
				toolName:     tool.Name,
				operation:    operation,
			}
		}
	}
	if match.operation.Name == "" {
		return sandboxForwardProxyOperationMatch{}, false, nil
	}
	return match, true, nil
}

func sandboxForwardProxyMatchErrorReason(err error) string {
	switch {
	case err == nil:
		return ""
	case strings.Contains(err.Error(), "invalid"):
		return "connector_specs_invalid"
	default:
		return "connector_specs_unavailable"
	}
}

func sandboxForwardProxyBodyRejectReason(err error) string {
	if err != nil && strings.Contains(err.Error(), "read failed") {
		return "request_body_read_failed"
	}
	return "request_body_too_large"
}

func sandboxForwardProxyUpstreamURL(targetHost string, decrypted *http.Request) (*url.URL, error) {
	requestURI := decrypted.URL.RequestURI()
	if requestURI == "" {
		requestURI = "/"
	}
	return parseSandboxProxyUpstreamURL("https://" + targetHost + requestURI)
}

func readSandboxForwardProxyRequestBody(decrypted *http.Request) ([]byte, string, error) {
	if decrypted.Body == nil {
		return nil, "", nil
	}
	body, err := io.ReadAll(io.LimitReader(decrypted.Body, sandboxForwardProxyMaxRequestBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("sandbox proxy decrypted request body read failed")
	}
	if len(body) > sandboxForwardProxyMaxRequestBytes {
		return nil, "", fmt.Errorf("sandbox proxy decrypted request body exceeded size limit")
	}
	if len(body) == 0 {
		return nil, "", nil
	}
	return body, strings.TrimSpace(decrypted.Header.Get("Content-Type")), nil
}

type sandboxForwardProxyCapture struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newSandboxForwardProxyCapture() *sandboxForwardProxyCapture {
	return &sandboxForwardProxyCapture{header: http.Header{}, status: http.StatusOK}
}

func (w *sandboxForwardProxyCapture) Header() http.Header {
	return w.header
}

func (w *sandboxForwardProxyCapture) WriteHeader(status int) {
	w.status = status
}

func (w *sandboxForwardProxyCapture) Write(p []byte) (int, error) {
	return w.body.Write(p)
}

func writeSandboxForwardProxyCaptured(conn io.Writer, sessionID, targetHost string, captured *sandboxForwardProxyCapture) {
	contentType := strings.TrimSpace(captured.header.Get("Content-Type"))
	if contentType == "" {
		contentType = "application/json"
	}
	writeSandboxForwardProxyResponse(conn, sessionID, targetHost, captured.status, contentType, captured.body.Bytes())
}

func writeSandboxForwardProxyError(conn io.Writer, status int, sessionID, targetHost, message string) {
	body := strings.TrimSpace(message) + "\n"
	writeSandboxForwardProxyResponse(conn, sessionID, targetHost, status, "text/plain; charset=utf-8", []byte(body))
}

func writeSandboxForwardProxyResponse(conn io.Writer, sessionID, targetHost string, status int, contentType string, body []byte) {
	if status == 0 {
		status = http.StatusInternalServerError
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	contentType = sandboxForwardProxyHeaderValue(contentType)
	sessionID = sandboxForwardProxyHeaderValue(sessionID)
	targetHost = sandboxForwardProxyHeaderValue(targetHost)
	_, _ = fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nConnection: close\r\nContent-Type: %s\r\nX-Aileron-Session-Id: %s\r\nX-Aileron-Proxy-Upstream-Host: %s\r\nContent-Length: %d\r\n\r\n", status, http.StatusText(status), contentType, sessionID, targetHost, len(body))
	if len(body) > 0 {
		_, _ = conn.Write(body)
	}
}

func sandboxForwardProxyHeaderValue(value string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func sandboxForwardProxyConnectHost(r *http.Request) (string, error) {
	target := strings.TrimSpace(r.Host)
	if target == "" {
		target = strings.TrimSpace(r.URL.Host)
	}
	if target == "" {
		target = strings.TrimSpace(r.RequestURI)
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil || strings.TrimSpace(host) == "" || port != "443" {
		return "", fmt.Errorf("sandbox HTTPS forward proxy CONNECT target must be host:443")
	}
	return host, nil
}

func (s *apiServer) sandboxForwardProxyCertificate(sessionID, host string) (tls.Certificate, error) {
	if strings.TrimSpace(s.sandboxProxyStateDir) == "" {
		return tls.Certificate{}, fmt.Errorf("sandbox proxy state dir is not configured")
	}
	root := filepath.Join(s.sandboxProxyStateDir, "sessions", sessionID, "sandbox-proxy")
	caPEM, err := os.ReadFile(filepath.Join(root, "ca.pem"))
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(root, "ca.key"))
	if err != nil {
		return tls.Certificate{}, err
	}
	caTLS, err := tls.X509KeyPair(caPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, err
	}
	ca, err := x509.ParseCertificate(caTLS.Certificate[0])
	if err != nil {
		return tls.Certificate{}, err
	}
	caKey, ok := caTLS.PrivateKey.(*rsa.PrivateKey)
	if !ok {
		return tls.Certificate{}, fmt.Errorf("sandbox proxy CA key is not RSA")
	}
	return signSandboxForwardProxyLeaf(ca, caKey, host)
}

func signSandboxForwardProxyLeaf(ca *x509.Certificate, caKey *rsa.PrivateKey, host string) (tls.Certificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now().UTC()
	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: host,
		},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(30 * time.Minute),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, ca, &key.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{
		Certificate: [][]byte{der, ca.Raw},
		PrivateKey:  key,
		Leaf:        &template,
	}, nil
}

func (s *apiServer) authenticateSandboxForwardProxy(w http.ResponseWriter, r *http.Request) (sandboxProxyAuth, bool) {
	auth, err := parseSandboxProxyAuthorization(r.Header.Get("Proxy-Authorization"))
	if err != nil {
		writeProxyAuthRequired(w, err.Error())
		return sandboxProxyAuth{}, false
	}
	if s.localDaemonToken != "" && auth.Token != s.localDaemonToken {
		writeProxyAuthRequired(w, "invalid sandbox proxy token")
		return sandboxProxyAuth{}, false
	}
	return auth, true
}

func parseSandboxProxyAuthorization(header string) (sandboxProxyAuth, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return sandboxProxyAuth{}, fmt.Errorf("missing sandbox proxy authorization")
	}
	const prefix = "Basic "
	if !strings.HasPrefix(header, prefix) {
		return sandboxProxyAuth{}, fmt.Errorf("sandbox proxy authorization must use Basic auth")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(strings.TrimPrefix(header, prefix)))
	if err != nil {
		return sandboxProxyAuth{}, fmt.Errorf("sandbox proxy authorization is invalid")
	}
	username, password, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return sandboxProxyAuth{}, fmt.Errorf("sandbox proxy authorization is invalid")
	}
	sessionID := strings.TrimSpace(username)
	if username != sessionID {
		return sandboxProxyAuth{}, fmt.Errorf("sandbox proxy session id is invalid")
	}
	if sessionID == "" {
		return sandboxProxyAuth{}, fmt.Errorf("sandbox proxy session id is required")
	}
	if err := validateSandboxProxySessionID(sessionID); err != nil {
		return sandboxProxyAuth{}, err
	}
	return sandboxProxyAuth{SessionID: sessionID, Token: password}, nil
}

func validateSandboxProxySessionID(sessionID string) error {
	if sessionID != strings.TrimSpace(sessionID) || strings.ContainsAny(sessionID, `/\:`) || sessionID == "." || sessionID == ".." {
		return fmt.Errorf("sandbox proxy session id is invalid")
	}
	return nil
}

func writeProxyAuthRequired(w http.ResponseWriter, message string) {
	w.Header().Set("Proxy-Authenticate", `Basic realm="`+sandboxForwardProxyRealm+`"`)
	http.Error(w, message, http.StatusProxyAuthRequired)
}
