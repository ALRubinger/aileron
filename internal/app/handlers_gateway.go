package app

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/ALRubinger/aileron/internal/failure"
)

// PostChatCompletions handles `POST /v1/chat/completions` as a
// transparent reverse proxy to the configured upstream OpenAI-shaped
// provider. Aileron does not augment the agent's tool catalog or
// intercept tool calls in the LLM stream — actions are exposed to the
// agent over MCP via `aileron-mcp` and executed through
// `POST /v1/actions/{name}/run`, which is the single chokepoint where
// the manifest's `[approval]` block is honored. Keeping the gateway
// purely pass-through guarantees there is one execution path for
// every action, regardless of which agent is launched.
//
// When the local credential vault is locked (per ADR-0011), the
// endpoint refuses to serve — the runtime cannot resolve any
// credential a connector might need on a subsequent MCP-driven action
// invocation. The agent receives a 423 FailureEnvelope with class
// `binding_required`, prompting them to surface the unlock requirement
// to the user.
func (s *apiServer) PostChatCompletions(w http.ResponseWriter, r *http.Request) {
	if s.vaultLocked {
		writeVaultLocked(w)
		return
	}
	if s.openAIProxy == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway_not_configured",
			"OpenAI gateway upstream is not configured")
		return
	}
	s.openAIProxy.ServeHTTP(w, r)
}

// PostMessages handles `POST /v1/messages` with the same pass-through
// semantics for the Anthropic Messages protocol.
func (s *apiServer) PostMessages(w http.ResponseWriter, r *http.Request) {
	if s.vaultLocked {
		writeVaultLocked(w)
		return
	}
	if s.anthropicProxy == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway_not_configured",
			"Anthropic gateway upstream is not configured")
		return
	}
	s.anthropicProxy.ServeHTTP(w, r)
}

// handleAnthropicAPI proxies Anthropic's `/api/*` surface to the
// configured Anthropic upstream. The motivating path is Claude Code's
// login-state probe, `GET /api/oauth/profile`: in api-key mode Claude
// Code points `ANTHROPIC_BASE_URL` at the daemon, so this probe lands
// here rather than at api.anthropic.com. Without this route it falls
// through to the webapp catch-all and Claude Code reports "not logged
// in" (#1696).
//
// Like PostMessages this is a transparent reverse proxy: path, method,
// body, and headers (including `x-api-key`) are forwarded unchanged, so
// the agent's own credential authenticates to Anthropic. The route is
// scoped to the `/api/` prefix, which is entirely Anthropic's namespace
// (Aileron's own API lives under `/v1/`). When no upstream is
// configured (anthropicAPIProxy nil) the endpoint returns 503, matching
// the gateway's not-configured contract.
func (s *apiServer) handleAnthropicAPI(w http.ResponseWriter, r *http.Request) {
	if s.anthropicAPIProxy == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway_not_configured",
			"Anthropic gateway upstream is not configured")
		return
	}
	s.anthropicAPIProxy.ServeHTTP(w, r)
}

// writeVaultLocked emits the canonical FailureEnvelope for "the
// runtime can't serve this request until the vault is unlocked."
// `binding_required` is the closest fit in ADR-0010's closed
// taxonomy: the user must bind a credential (their passphrase) and
// retry. failure.WriteHTTP maps the class to its canonical status
// (412 Precondition Failed for binding_required).
func writeVaultLocked(w http.ResponseWriter) {
	f := failure.BindingRequiredFailure(
		"local credential vault is locked; run `aileron vault unlock` to resume",
		failure.WithDetails(map[string]any{
			"required": "vault.passphrase",
			"hint":     "the runtime needs the KEK in memory before any connector credential can be resolved",
		}),
	)
	failure.WriteHTTP(w, f)
}

// newGatewayProxy builds an HTTP reverse proxy that forwards requests
// to the configured upstream LLM provider.
//
// The agent's `Authorization` (OpenAI) or `x-api-key`/`anthropic-version`
// (Anthropic) headers ride through unchanged — Aileron does not
// authenticate to upstream itself; the agent's own credentials flow
// through. FlushInterval = -1 ensures SSE chunks reach the agent as
// soon as the upstream emits them, preserving streaming semantics.
//
// providerLabel is used for structured logs only; it does not affect
// request routing.
//
// The returned handler wraps the proxy's ResponseWriter so a mid-
// stream write failure (server-side deadline firing, agent
// disconnecting partway through SSE) emits a single Warn log line
// with the bytes-written / elapsed-ms / r.Context() error. The
// proxy's own ErrorHandler only triggers for pre-response transport
// failures; without this trap the request log shows status=200 even
// when the stream got severed and the agent saw a "socket connection
// was closed unexpectedly".
func newGatewayProxy(upstream *url.URL, providerLabel string, log *slog.Logger) http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.Out.Host = upstream.Host
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.WarnContext(r.Context(), "gateway: upstream request failed",
				"provider", providerLabel,
				"upstream", upstream.String(),
				"error", err)
			writeError(w, http.StatusBadGateway, "upstream_error",
				"upstream provider request failed: "+err.Error())
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &streamWatchWriter{
			ResponseWriter: w,
			log:            log,
			provider:       providerLabel,
			upstream:       upstream.String(),
			ctx:            r.Context(),
			start:          time.Now(),
		}
		rp.ServeHTTP(sw, r)
	})
}

// streamWatchWriter wraps an http.ResponseWriter to surface mid-
// stream write failures into the daemon log. Once the upstream is
// past response headers, httputil.ReverseProxy's ErrorHandler no
// longer fires — any later write-side failure (the http.Server's
// WriteTimeout firing, the agent dropping the socket, etc.) returns
// up the response-copy loop and silently terminates the stream.
// This wrapper logs once per request when that happens.
type streamWatchWriter struct {
	http.ResponseWriter
	log      *slog.Logger
	provider string
	upstream string
	ctx      context.Context
	start    time.Time
	bytes    int64
	logged   bool
}

func (w *streamWatchWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	if err != nil && !w.logged {
		w.logged = true
		w.log.WarnContext(w.ctx, "gateway: response stream interrupted",
			"provider", w.provider,
			"upstream", w.upstream,
			"bytes_written", w.bytes,
			"elapsed_ms", time.Since(w.start).Milliseconds(),
			"ctx_err", w.ctx.Err(),
			"error", err)
	}
	return n, err
}

// Flush proxies through to the underlying ResponseWriter when it
// implements http.Flusher. Required so httputil.ReverseProxy's
// FlushInterval=-1 mode can flush SSE chunks through this wrapper —
// without it, the proxy's type assertion fails and streaming
// silently buffers until end-of-response.
func (w *streamWatchWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
