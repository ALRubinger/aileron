package app

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/internal/auth"
)

type contextKey int

const requestIDKey contextKey = iota

// RequestID returns the request ID from the context, or empty string if none.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// requestIDMiddleware injects a unique request ID into each request context
// and sets it on the response. If the client sends X-Request-ID, it is reused.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = generateID()
		}
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// loggingMiddleware logs each request with method, path, status, and duration.
func loggingMiddleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.InfoContext(r.Context(), "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", RequestID(r.Context()),
		)
	})
}

// statusWriter wraps http.ResponseWriter to capture the status code.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

// Flush proxies through to the underlying ResponseWriter when it
// implements http.Flusher. Required for SSE handlers that assert
// `w.(http.Flusher)` — without this, the type assertion fails on the
// wrapped writer and the stream never starts.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

// corsMiddleware adds CORS headers, echoing the request Origin so that
// credentialed (cookie-based) requests work. The wildcard "*" is not
// allowed when credentials: "include" is used by the browser.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-ID")
		w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID")
		w.Header().Set("Vary", "Origin")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

const localDaemonTokenCookie = "aileron_daemon_token"

// newCaveatIssuer builds a [auth.CaveatIssuer] over the per-daemon
// signing key, or returns nil when no key is configured (caveat tokens
// disabled — only the master token is accepted). The issuer string
// matches the master-token model: there is exactly one local daemon.
func newCaveatIssuer(signingKey []byte) *auth.CaveatIssuer {
	if len(signingKey) == 0 {
		return nil
	}
	return auth.NewCaveatIssuer(signingKey, "aileron-daemon")
}

// localDaemonAuthMiddleware protects the local daemon's /v1/* surface
// with the token advertised in daemon.json. CLI/shim clients send it as
// Authorization: Bearer <token>; the same-origin webapp gets a cookie
// through /v1/auth/handshake so browser EventSource can authenticate.
//
// A bearer equal to the master token grants full, unscoped access (host
// CLI + webapp path, unchanged). Otherwise the middleware attempts to
// validate the bearer as a session caveat token (ADR-0024, #958): on
// success it enforces a route->capability map and the token's session
// binding, so the in-container aileron-mcp reaches only the minimal
// surface it needs. Anything else is 401.
func localDaemonAuthMiddleware(token string, caveats *auth.CaveatIssuer, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions || !strings.HasPrefix(r.URL.Path, "/v1/") || localDaemonAuthSkip(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		got := bearerToken(r)
		if got == "" {
			if c, err := r.Cookie(localDaemonTokenCookie); err == nil {
				got = c.Value
			}
		}
		// Master token: full unscoped access (host CLI + webapp cookie).
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		// Otherwise: try the bearer as a session caveat token.
		if caveats != nil && got != "" {
			if claims, err := caveats.Validate(got); err == nil {
				if status, code, msg := authorizeCaveat(claims, r.Method, r.URL.Path); status != http.StatusOK {
					writeError(w, status, code, msg)
					return
				}
				next.ServeHTTP(w, r)
				return
			}
		}
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid daemon token")
	})
}

// authorizeCaveat enforces the caveat token's route->capability map and
// session binding for the given request. It returns http.StatusOK with
// empty code/msg when the request is permitted; otherwise the HTTP
// status, error code, and message to return.
//
// Route map (ADR-0024, #958):
//   - GET  /v1/actions, GET /v1/actions/{name}      -> actions:list
//   - POST /v1/actions/{name}/run                   -> actions:run
//   - GET  /v1/action-approvals/{id}/result         -> approvals:poll
//   - any  /v1/sessions/{id}/comms/*                 -> comms (and the
//     path's {id} must equal the token's bound session)
//
// A path the caveat vocabulary does not cover is 403: the caveat token
// is scoped strictly below the master token and may not reach it.
func authorizeCaveat(claims *auth.CaveatClaims, method, path string) (status int, code, msg string) {
	const forbidden = http.StatusForbidden

	// Session comms: /v1/sessions/{session_id}/comms/...
	if rest, ok := strings.CutPrefix(path, "/v1/sessions/"); ok {
		segs := strings.SplitN(rest, "/", 2)
		if len(segs) == 2 && strings.HasPrefix(segs[1], "comms/") {
			sessionID := segs[0]
			if !claims.HasCap(auth.CapComms) {
				return forbidden, "forbidden", "caveat token lacks the comms capability"
			}
			// Defense in depth: reject another session's comms path.
			if sessionID != claims.Session {
				return forbidden, "forbidden", "caveat token is bound to a different session"
			}
			return http.StatusOK, "", ""
		}
		// Other /v1/sessions/* paths are not in the caveat vocabulary.
		return forbidden, "forbidden", "caveat token does not authorize this route"
	}

	// Action-approval result poll: GET /v1/action-approvals/{id}/result
	if method == http.MethodGet &&
		strings.HasPrefix(path, "/v1/action-approvals/") &&
		strings.HasSuffix(path, "/result") {
		if !claims.HasCap(auth.CapApprovalsPoll) {
			return forbidden, "forbidden", "caveat token lacks the approvals:poll capability"
		}
		return http.StatusOK, "", ""
	}

	// Action run: POST /v1/actions/{name}/run
	if method == http.MethodPost &&
		strings.HasPrefix(path, "/v1/actions/") &&
		strings.HasSuffix(path, "/run") {
		if !claims.HasCap(auth.CapActionsRun) {
			return forbidden, "forbidden", "caveat token lacks the actions:run capability"
		}
		return http.StatusOK, "", ""
	}

	// Action list/detail: GET /v1/actions, GET /v1/actions/{name}
	if method == http.MethodGet &&
		(path == "/v1/actions" || strings.HasPrefix(path, "/v1/actions/")) {
		if !claims.HasCap(auth.CapActionsList) {
			return forbidden, "forbidden", "caveat token lacks the actions:list capability"
		}
		return http.StatusOK, "", ""
	}

	return forbidden, "forbidden", "caveat token does not authorize this route"
}

func localDaemonAuthSkip(path string) bool {
	switch path {
	case "/v1/health", "/v1/auth/handshake":
		return true
	case "/v1/messages", "/v1/chat/completions":
		// The LLM gateway is a transparent passthrough: the reverse
		// proxy forwards the agent's own `x-api-key` /
		// `Authorization` headers to upstream Anthropic / OpenAI
		// unchanged (see newGatewayProxy in handlers_gateway.go), and
		// the daemon never substitutes credentials of its own. Gating
		// these paths on the daemon's bearer token would reject every
		// in-container agent whose Authorization header carries its
		// own provider OAuth token (the daemon would compare it to
		// its own daemon token, see a mismatch, and 401 before the
		// proxy ever runs). That 401 is also indistinguishable to the
		// agent from an upstream auth failure, which sends users
		// chasing the wrong root cause. The gateway adds no upstream
		// credentials of its own, so daemon-token gating provides no
		// security benefit here — exempt it.
		return true
	default:
		return false
	}
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, prefix))
}

func generateID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
