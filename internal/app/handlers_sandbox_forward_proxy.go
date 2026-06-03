package app

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
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
	w.Header().Set("X-Aileron-Session-Id", auth.SessionID)
	http.Error(w, "sandbox HTTPS forward proxy transport is not implemented yet; tracked by issue #896", http.StatusNotImplemented)
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
