package app

import (
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	req.Header.Set("Proxy-Authorization", basicProxyAuth("session-123", "daemon-token"))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Aileron-Session-Id"); got != "session-123" {
		t.Fatalf("X-Aileron-Session-Id = %q, want session-123", got)
	}
	if !strings.Contains(rec.Body.String(), "sandbox HTTPS forward proxy transport is not implemented yet") {
		t.Fatalf("body = %q", rec.Body.String())
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
	handler, err := NewHandlerWithConfig(slog.New(slog.NewJSONHandler(io.Discard, nil)), Config{
		Vault:            vault.NewMemVault(),
		LocalDaemonToken: token,
	})
	if err != nil {
		t.Fatalf("NewHandlerWithConfig: %v", err)
	}
	return handler
}

func basicProxyAuth(sessionID, token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(sessionID+":"+token))
}
