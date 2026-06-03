package app

import (
	"bufio"
	"bytes"
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
	w.Header().Set("X-Aileron-Session-Id", auth.SessionID)
	http.Error(w, "sandbox HTTPS forward proxy transport is not implemented yet; tracked by issue #896", http.StatusNotImplemented)
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
		w.Header().Set("X-Aileron-Session-Id", auth.SessionID)
		http.Error(w, "sandbox HTTPS forward proxy CONNECT transport is not available; tracked by issue #896", http.StatusNotImplemented)
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

func (s *apiServer) handleSandboxForwardProxyDecrypted(conn io.Writer, decrypted *http.Request, auth sandboxProxyAuth, targetHost string) {
	upstream, err := sandboxForwardProxyUpstreamURL(targetHost, decrypted)
	if err != nil {
		writeSandboxForwardProxyError(conn, http.StatusBadRequest, auth.SessionID, targetHost, "sandbox proxy decrypted request target is invalid")
		return
	}
	match, ok, err := s.matchSandboxForwardProxyOperation(decrypted.Method, upstream)
	if err != nil {
		if errors.Is(err, errSandboxForwardProxyAmbiguousOperation) {
			writeSandboxForwardProxyError(conn, http.StatusForbidden, auth.SessionID, targetHost, err.Error())
			return
		}
		writeSandboxForwardProxyError(conn, http.StatusInternalServerError, auth.SessionID, targetHost, err.Error())
		return
	}
	if !ok {
		writeSandboxForwardProxyError(conn, http.StatusForbidden, auth.SessionID, targetHost, "sandbox proxy decrypted request did not match an installed connector operation")
		return
	}
	body, contentType, err := readSandboxForwardProxyRequestBody(decrypted)
	if err != nil {
		writeSandboxForwardProxyError(conn, http.StatusRequestEntityTooLarge, auth.SessionID, targetHost, err.Error())
		return
	}

	captured := newSandboxForwardProxyCapture()
	result, ok := s.executeSandboxProxyRequest(captured, decrypted, match.connectorFQN, match.toolName, decrypted.Method, upstream, match.operation, body, contentType)
	if !ok {
		writeSandboxForwardProxyCaptured(conn, auth.SessionID, targetHost, captured)
		return
	}
	writeSandboxForwardProxyResponse(conn, auth.SessionID, targetHost, result.UpstreamStatus, derefString(result.ContentType), result.BodyBase64)
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
