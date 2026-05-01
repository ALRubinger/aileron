package oauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// CallbackResult is the data extracted from the provider's redirect
// to the loopback listener. State is returned verbatim so the caller
// can compare it against the value sent at the authorize step (the
// listener does not know what value to expect).
type CallbackResult struct {
	Code  string
	State string
}

// Listener captures the OAuth callback on a loopback HTTP server.
// One instance handles exactly one callback then shuts down.
type Listener struct {
	listener net.Listener
	server   *http.Server
	done     chan callbackEvent
}

type callbackEvent struct {
	res CallbackResult
	err error
}

// NewListener binds an `http.Server` on `127.0.0.1:port` (use port 0
// to let the OS pick a free port — read the port back from
// [Listener.Port]). The server serves a single endpoint at
// `/callback` that captures `code` + `state` query params and renders
// a "you can close this tab" HTML page.
//
// Lifecycle:
//
//   - [Listener.Port] returns the bound port (useful when the caller
//     passed 0).
//   - [Listener.Await] blocks until the first callback arrives or
//     ctx is cancelled.
//   - [Listener.Close] shuts down the server. Idempotent; safe to
//     defer.
func NewListener(port int) (*Listener, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	tcp, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("oauth: bind callback listener: %w", err)
	}
	l := &Listener{
		listener: tcp,
		done:     make(chan callbackEvent, 1),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", l.handleCallback)
	l.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := l.server.Serve(tcp); err != nil && !errors.Is(err, http.ErrServerClosed) {
			l.tryDone(callbackEvent{err: fmt.Errorf("oauth: callback server: %w", err)})
		}
	}()
	return l, nil
}

// Port returns the bound TCP port — useful when the caller passed 0
// to NewListener and let the OS choose. Suitable for substituting
// into a redirect_uri before opening the user's browser.
func (l *Listener) Port() int {
	return l.listener.Addr().(*net.TCPAddr).Port
}

// RedirectURI returns the canonical loopback redirect URI for the
// listener's port: `http://127.0.0.1:<port>/callback`. Suitable for
// the `redirect_uri` parameter in the authorize URL and the token
// exchange request.
func (l *Listener) RedirectURI() string {
	return fmt.Sprintf("http://127.0.0.1:%d/callback", l.Port())
}

// Await blocks until the OAuth callback arrives or ctx is cancelled.
// Returns the captured `code` + `state`. The listener consumes
// exactly one callback then continues running until [Listener.Close]
// is called.
func (l *Listener) Await(ctx context.Context) (CallbackResult, error) {
	select {
	case ev := <-l.done:
		return ev.res, ev.err
	case <-ctx.Done():
		return CallbackResult{}, ctx.Err()
	}
}

// Close shuts down the loopback HTTP server. Safe to call multiple
// times; subsequent calls are no-ops. Use a deferred Close to
// guarantee the listener is torn down even on error paths.
func (l *Listener) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	return l.server.Shutdown(ctx)
}

// handleCallback serves /callback once. Captures the query params,
// pushes them onto the done channel, and renders a static success
// page. Errors (missing code, error param) push an error event.
func (l *Listener) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if errMsg := q.Get("error"); errMsg != "" {
		desc := q.Get("error_description")
		l.tryDone(callbackEvent{err: fmt.Errorf("oauth: provider returned error %q: %s", errMsg, desc)})
		writeCallbackErrorPage(w, errMsg, desc)
		return
	}
	code := q.Get("code")
	state := q.Get("state")
	if code == "" {
		l.tryDone(callbackEvent{err: fmt.Errorf("oauth: callback missing code parameter")})
		writeCallbackErrorPage(w, "missing_code", "The provider redirected without a code parameter.")
		return
	}
	l.tryDone(callbackEvent{res: CallbackResult{Code: code, State: state}})
	writeCallbackSuccessPage(w)
}

// tryDone publishes an event to the done channel without blocking.
// Subsequent events (e.g. a malicious replay or browser refresh) are
// dropped — only the first callback is consumed.
func (l *Listener) tryDone(ev callbackEvent) {
	select {
	case l.done <- ev:
	default:
	}
}

func writeCallbackSuccessPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html>
<html><head><meta charset="utf-8"><title>Aileron — Authorized</title></head>
<body style="font-family:system-ui,-apple-system,sans-serif;max-width:32rem;margin:6rem auto;padding:0 1rem;color:#222">
<h1 style="font-size:1.25rem">Authorized</h1>
<p>The Aileron CLI now has the credential. You can close this tab and return to your terminal.</p>
</body></html>`))
}

func writeCallbackErrorPage(w http.ResponseWriter, code, description string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = fmt.Fprintf(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>Aileron — Authorization failed</title></head>
<body style="font-family:system-ui,-apple-system,sans-serif;max-width:32rem;margin:6rem auto;padding:0 1rem;color:#222">
<h1 style="font-size:1.25rem">Authorization failed</h1>
<p><strong>%s</strong></p>
<p>%s</p>
<p>Return to your terminal — the Aileron CLI surfaced the error there.</p>
</body></html>`, htmlEscape(code), htmlEscape(description))
}

// htmlEscape provides minimal escaping for the small substitutions
// in the callback pages. Avoids pulling in html/template just for
// this.
func htmlEscape(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '<':
			out = append(out, []byte("&lt;")...)
		case '>':
			out = append(out, []byte("&gt;")...)
		case '&':
			out = append(out, []byte("&amp;")...)
		case '"':
			out = append(out, []byte("&quot;")...)
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}
