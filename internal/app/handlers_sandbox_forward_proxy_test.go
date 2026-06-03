package app

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/vault"
)

func TestSandboxForwardProxy_CONNECTRequiresProxyAuthorization(t *testing.T) {
	handler := newSandboxForwardProxyTestHandler(t, "daemon-token")
	req := httptest.NewRequest(http.MethodConnect, "http://gmail.googleapis.com:443", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusProxyAuthRequired {
		t.Fatalf("status = %d, want 407; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Proxy-Authenticate"); got != `Basic realm="aileron sandbox proxy"` {
		t.Fatalf("Proxy-Authenticate = %q", got)
	}
}

func TestSandboxForwardProxy_CONNECTRejectsInvalidToken(t *testing.T) {
	handler := newSandboxForwardProxyTestHandler(t, "daemon-token")
	req := httptest.NewRequest(http.MethodConnect, "http://gmail.googleapis.com:443", nil)
	req.Header.Set("Proxy-Authorization", basicProxyAuth("session-123", "wrong-token"))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusProxyAuthRequired {
		t.Fatalf("status = %d, want 407; body=%s", rec.Code, rec.Body.String())
	}
}

func TestSandboxForwardProxy_CONNECTAuthenticatesAndFailsClosed(t *testing.T) {
	handler := newSandboxForwardProxyTestHandler(t, "daemon-token")
	req := httptest.NewRequest(http.MethodConnect, "http://gmail.googleapis.com:443", nil)
	req.Host = "gmail.googleapis.com:443"
	req.Header.Set("Proxy-Authorization", basicProxyAuth("session-123", "daemon-token"))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Aileron-Session-Id"); got != "session-123" {
		t.Fatalf("X-Aileron-Session-Id = %q, want session-123", got)
	}
	if !strings.Contains(rec.Body.String(), "sandbox HTTPS forward proxy CONNECT transport is not available") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestSandboxForwardProxy_CONNECTTerminatesTLSAndFailsClosedAfterDecryptedRequest(t *testing.T) {
	stateDir := t.TempDir()
	caPEM := writeSandboxProxyTestCA(t, stateDir, "session-123")
	handler := newSandboxForwardProxyTestHandlerWithStateDir(t, "daemon-token", stateDir)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to append CA cert")
	}
	proxyURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		ProxyConnectHeader: http.Header{
			"Proxy-Authorization": []string{basicProxyAuth("session-123", "daemon-token")},
		},
		TLSClientConfig: &tls.Config{
			RootCAs:    roots,
			MinVersion: tls.VersionTLS12,
		},
	}}
	resp, err := client.Get("https://api.example.test/v1/resource?secret=not-audited")
	if err != nil {
		t.Fatalf("GET through sandbox forward proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Aileron-Session-Id"); got != "session-123" {
		t.Fatalf("X-Aileron-Session-Id = %q, want session-123", got)
	}
	if got := resp.Header.Get("X-Aileron-Proxy-Upstream-Host"); got != "api.example.test" {
		t.Fatalf("X-Aileron-Proxy-Upstream-Host = %q, want api.example.test", got)
	}
}

func TestSandboxForwardProxy_CONNECTRejectsInvalidTarget(t *testing.T) {
	handler := newSandboxForwardProxyTestHandler(t, "daemon-token")
	req := httptest.NewRequest(http.MethodConnect, "/", nil)
	req.Host = "api.example.test:8443"
	req.Header.Set("Proxy-Authorization", basicProxyAuth("session-123", "daemon-token"))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestSandboxForwardProxy_CONNECTRequiresHijackerAfterSessionCA(t *testing.T) {
	stateDir := t.TempDir()
	writeSandboxProxyTestCA(t, stateDir, "session-123")
	srv := &apiServer{localDaemonToken: "daemon-token", sandboxProxyStateDir: stateDir}
	req := httptest.NewRequest(http.MethodConnect, "/", nil)
	req.Host = "api.example.test:443"
	req.Header.Set("Proxy-Authorization", basicProxyAuth("session-123", "daemon-token"))
	rec := newNoHijackResponseWriter()

	srv.HandleSandboxForwardProxy(rec, req)

	if rec.code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.code, rec.body.String())
	}
}

func TestSandboxForwardProxyConnectHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodConnect, "/", nil)
	req.Host = "api.example.test:443"
	got, err := sandboxForwardProxyConnectHost(req)
	if err != nil {
		t.Fatalf("sandboxForwardProxyConnectHost: %v", err)
	}
	if got != "api.example.test" {
		t.Fatalf("host = %q, want api.example.test", got)
	}

	req = httptest.NewRequest(http.MethodConnect, "/", nil)
	req.Host = "api.example.test:8443"
	if _, err := sandboxForwardProxyConnectHost(req); err == nil {
		t.Fatal("expected non-443 CONNECT target to be rejected")
	}
}

func TestSandboxForwardProxyCertificateErrors(t *testing.T) {
	srv := &apiServer{}
	if _, err := srv.sandboxForwardProxyCertificate("session-123", "api.example.test"); err == nil {
		t.Fatal("expected missing state dir error")
	}

	srv.sandboxProxyStateDir = t.TempDir()
	if _, err := srv.sandboxForwardProxyCertificate("session-123", "api.example.test"); err == nil {
		t.Fatal("expected missing CA file error")
	}

	root := filepath.Join(srv.sandboxProxyStateDir, "sessions", "session-123", "sandbox-proxy")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create CA dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "ca.pem"), []byte("not a cert"), 0o644); err != nil {
		t.Fatalf("write invalid CA cert: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "ca.key"), []byte("not a key"), 0o600); err != nil {
		t.Fatalf("write invalid CA key: %v", err)
	}
	if _, err := srv.sandboxForwardProxyCertificate("session-123", "api.example.test"); err == nil {
		t.Fatal("expected invalid CA/key error")
	}
}

func TestSignSandboxForwardProxyLeaf_IncludesIPSAN(t *testing.T) {
	ca, key := sandboxProxyTestCA(t)
	cert, err := signSandboxForwardProxyLeaf(ca, key, "127.0.0.1")
	if err != nil {
		t.Fatalf("signSandboxForwardProxyLeaf: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if len(leaf.IPAddresses) != 1 || leaf.IPAddresses[0].String() != "127.0.0.1" {
		t.Fatalf("IP SANs = %v, want 127.0.0.1", leaf.IPAddresses)
	}
}

func TestSandboxForwardProxy_AbsoluteFormV1PathDoesNotHitDaemonAuth(t *testing.T) {
	handler := newSandboxForwardProxyTestHandler(t, "daemon-token")
	req := httptest.NewRequest(http.MethodGet, "https://api.example.test/v1/resource", nil)
	req.Header.Set("Proxy-Authorization", basicProxyAuth("session-123", "daemon-token"))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Aileron-Session-Id"); got != "session-123" {
		t.Fatalf("X-Aileron-Session-Id = %q, want session-123", got)
	}
}

func TestSandboxForwardProxy_DoesNotInterceptNormalV1Routes(t *testing.T) {
	handler := newSandboxForwardProxyTestHandler(t, "daemon-token")
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestParseSandboxProxyAuthorization(t *testing.T) {
	got, err := parseSandboxProxyAuthorization(basicProxyAuth("session-123", "daemon-token"))
	if err != nil {
		t.Fatalf("parseSandboxProxyAuthorization: %v", err)
	}
	if got.SessionID != "session-123" || got.Token != "daemon-token" {
		t.Fatalf("auth = %+v", got)
	}
}

func TestParseSandboxProxyAuthorization_RejectsUnsafeSessionID(t *testing.T) {
	for _, sessionID := range []string{"nested/session", `nested\session`, " session-123", ".", ".."} {
		t.Run(sessionID, func(t *testing.T) {
			if _, err := parseSandboxProxyAuthorization(basicProxyAuth(sessionID, "daemon-token")); err == nil {
				t.Fatal("expected unsafe session id error")
			}
		})
	}
}

func TestParseSandboxProxyAuthorization_RejectsMalformedHeader(t *testing.T) {
	tests := map[string]string{
		"missing":     "",
		"not basic":   "Bearer token",
		"bad base64":  "Basic !!!",
		"missing sep": "Basic " + base64.StdEncoding.EncodeToString([]byte("session-123")),
		"empty user":  "Basic " + base64.StdEncoding.EncodeToString([]byte(":daemon-token")),
	}
	for name, header := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSandboxProxyAuthorization(header); err == nil {
				t.Fatal("expected malformed header error")
			}
		})
	}
}

func newSandboxForwardProxyTestHandler(t *testing.T, token string) http.Handler {
	t.Helper()
	return newSandboxForwardProxyTestHandlerWithStateDir(t, token, "")
}

func newSandboxForwardProxyTestHandlerWithStateDir(t *testing.T, token, stateDir string) http.Handler {
	t.Helper()
	handler, err := NewHandlerWithConfig(slog.New(slog.NewJSONHandler(io.Discard, nil)), Config{
		Vault:                vault.NewMemVault(),
		LocalDaemonToken:     token,
		SandboxProxyStateDir: stateDir,
	})
	if err != nil {
		t.Fatalf("NewHandlerWithConfig: %v", err)
	}
	return handler
}

func basicProxyAuth(sessionID, token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(sessionID+":"+token))
}

type noHijackResponseWriter struct {
	header http.Header
	body   strings.Builder
	code   int
}

func newNoHijackResponseWriter() *noHijackResponseWriter {
	return &noHijackResponseWriter{header: http.Header{}, code: http.StatusOK}
}

func (w *noHijackResponseWriter) Header() http.Header {
	return w.header
}

func (w *noHijackResponseWriter) WriteHeader(code int) {
	w.code = code
}

func (w *noHijackResponseWriter) Write(p []byte) (int, error) {
	return w.body.Write(p)
}

func writeSandboxProxyTestCA(t *testing.T, stateDir, sessionID string) []byte {
	t.Helper()
	ca, key := sandboxProxyTestCA(t)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})
	root := filepath.Join(stateDir, "sessions", sessionID, "sandbox-proxy")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create CA dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "ca.pem"), caPEM, 0o644); err != nil {
		t.Fatalf("write CA cert: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(filepath.Join(root, "ca.key"), keyPEM, 0o600); err != nil {
		t.Fatalf("write CA key: %v", err)
	}
	return caPEM
}

func sandboxProxyTestCA(t *testing.T) (*x509.Certificate, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		t.Fatalf("generate CA serial: %v", err)
	}
	now := time.Now().UTC()
	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "Aileron sandbox session CA",
			Organization: []string{"Aileron"},
		},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(12 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	return ca, key
}
