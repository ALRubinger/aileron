package app

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const sandboxForwardProxyRealm = "aileron sandbox proxy"

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
	_ = decrypted.Body.Close()

	body := "sandbox HTTPS forward proxy decrypted request routing is not implemented yet; tracked by issue #896\n"
	_, _ = fmt.Fprintf(tlsConn, "HTTP/1.1 501 Not Implemented\r\nConnection: close\r\nContent-Type: text/plain; charset=utf-8\r\nX-Aileron-Session-Id: %s\r\nX-Aileron-Proxy-Upstream-Host: %s\r\nContent-Length: %d\r\n\r\n%s", auth.SessionID, targetHost, len(body), body)
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
