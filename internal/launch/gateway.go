package launch

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/ALRubinger/aileron/internal/app"
	"github.com/ALRubinger/aileron/internal/approval"
	"github.com/ALRubinger/aileron/internal/vault"
)

// Gateway is an embedded Aileron control plane HTTP server bound to an
// ephemeral localhost port. The launch product starts one per session
// so the gateway uses the same passphrase-unlocked vault the user has
// just authenticated against, and the agent's LLM calls flow through
// the augment + intercept pipeline ratified by ADR-0008.
//
// The server binds to 127.0.0.1 only — there is no external exposure.
// The chosen port is reported via URL so the launcher can set the
// agent's LLM-endpoint env var (e.g. ANTHROPIC_BASE_URL) and pass it
// to aileron-mcp via AILERON_URL.
type Gateway struct {
	// URL is the base URL the agent should target — e.g.
	// "http://127.0.0.1:54321". The /v1 path suffix is the agent's
	// responsibility (Anthropic SDK appends "/v1/messages"; OpenAI SDK
	// appends "/v1/chat/completions").
	URL string

	server *http.Server
	log    *slog.Logger
}

// gatewayConfig holds the inputs StartGateway needs. Replaces the
// previous (vault, log) positional signature so the caller can run
// the gateway in either of two modes:
//
//   - "vault unlocked at startup" — supply Vault, leave LocalVaultPath
//     empty. The legacy stderr-prompt path (`AILERON_VAULT_PROMPT=stderr`).
//
//   - "vault locked, awaiting webapp unlock" — leave Vault nil, supply
//     LocalVaultPath. The default under #429: the daemon starts
//     vault-locked, the user submits their passphrase via the webapp
//     modal, and the daemon transitions to unlocked.
//
// Setting both is supported for tests that pre-unlock a vault but
// still want the /v1/vault/unlock handler wired (e.g. to assert
// "already unlocked → 409").
type gatewayConfig struct {
	Vault           vault.Vault
	LocalVaultPath  string
	ActionApprovals *approval.ActionApprovalQueue
	Log             *slog.Logger
}

// StartGateway constructs an Aileron app handler, binds it to an
// ephemeral localhost port, and starts serving in a background
// goroutine. The returned Gateway must be shut down via Close at
// session end.
//
// When cfg.Vault is supplied the daemon is unlocked from the start.
// When cfg.LocalVaultPath is supplied without cfg.Vault, the daemon
// starts vault-locked and exposes /v1/vault/unlock; vault-needing
// endpoints return 423 Locked until the user submits their
// passphrase via the webapp modal (#429).
func StartGateway(ctx context.Context, cfg gatewayConfig) (*Gateway, error) {
	if cfg.Vault == nil && cfg.LocalVaultPath == "" {
		return nil, fmt.Errorf("gateway: either Vault or LocalVaultPath is required")
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}

	// Allocate the listener first so the gateway's own URL is known by
	// the time the handler is constructed. The handler stamps that URL
	// into action-approval notifications via Config.WebappURL — once
	// the daemon serves ui/build directly, that URL is the canonical
	// webapp entry point and notifications carry a working
	// click-through target out of the box. Closing the listener on
	// any subsequent error keeps the port from leaking.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("gateway: binding listener: %w", err)
	}
	addr := listener.Addr().(*net.TCPAddr)
	url := fmt.Sprintf("http://127.0.0.1:%d", addr.Port)

	// Under launch, MCP is the canonical surface for action discovery
	// (cmd/aileron-mcp queries /v1/actions and routes tools/call to
	// /v1/actions/{name}/run). The gateway therefore disables in-band
	// tool augmentation so the LLM doesn't see duplicate action tools
	// (one set from MCP discovery, one from gateway augmentation). The
	// gateway remains in the path as a pass-through proxy and as the
	// substrate for Phase 2 request mediation.
	handler, err := app.NewHandlerWithConfig(log, app.Config{
		Vault:               cfg.Vault,
		LocalVaultPath:      cfg.LocalVaultPath,
		ActionApprovals:     cfg.ActionApprovals,
		DisableAugmentation: true,
		WebappURL:           url,
	})
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("gateway: building handler: %w", err)
	}

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Error("gateway: serve error", "error", err)
		}
	}()

	log.Info("embedded gateway listening", "url", url)

	return &Gateway{
		URL:    url,
		server: srv,
		log:    log,
	}, nil
}

// Close shuts down the embedded gateway with a short grace period.
// Safe to call on a nil receiver.
func (g *Gateway) Close(ctx context.Context) error {
	if g == nil || g.server == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return g.server.Shutdown(shutdownCtx)
}

// gatewayURL returns the running gateway's URL, or "" when no gateway
// is running. Convenience helper so call sites don't have to nil-check
// before dereferencing.
func gatewayURL(g *Gateway) string {
	if g == nil {
		return ""
	}
	return g.URL
}

// composeAgentEnv merges the agent's static env with the LLM-endpoint
// override variable when both an env-var name and a gateway URL are
// provided. Returns the input map unchanged when either is empty (i.e.
// when the agent doesn't support endpoint override or no gateway is
// running). Always returns a fresh map when adding an entry, so callers
// don't accidentally mutate the agent's static env.
func composeAgentEnv(agentEnv map[string]string, llmEnvVar, gatewayURL string) map[string]string {
	if llmEnvVar == "" || gatewayURL == "" {
		return agentEnv
	}
	out := make(map[string]string, len(agentEnv)+1)
	for k, v := range agentEnv {
		out[k] = v
	}
	out[llmEnvVar] = gatewayURL
	return out
}
