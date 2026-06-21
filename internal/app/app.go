// Package app wires together the Aileron control plane components and exposes
// them as an http.Handler. It is imported by the standalone server binary.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/internal/account"
	"github.com/ALRubinger/aileron/internal/action"
	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/approval"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/auth"
	"github.com/ALRubinger/aileron/internal/auth/capture"
	githubauth "github.com/ALRubinger/aileron/internal/auth/github"
	googleauth "github.com/ALRubinger/aileron/internal/auth/google"
	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/cli/unitloader"
	"github.com/ALRubinger/aileron/internal/comms"
	"github.com/ALRubinger/aileron/internal/config"
	"github.com/ALRubinger/aileron/internal/connector"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/draft"
	"github.com/ALRubinger/aileron/internal/hub"
	"github.com/ALRubinger/aileron/internal/notify"
	"github.com/ALRubinger/aileron/internal/observability"
	"github.com/ALRubinger/aileron/internal/policy"
	"github.com/ALRubinger/aileron/internal/proxybinding"
	"github.com/ALRubinger/aileron/internal/sandbox"
	sandboxcomposition "github.com/ALRubinger/aileron/internal/sandbox/composition"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
	"github.com/ALRubinger/aileron/internal/sessions"
	"github.com/ALRubinger/aileron/internal/source"
	calendarsource "github.com/ALRubinger/aileron/internal/source/calendar"
	githubsource "github.com/ALRubinger/aileron/internal/source/github"
	gmailsource "github.com/ALRubinger/aileron/internal/source/gmail"
	"github.com/ALRubinger/aileron/internal/store"
	"github.com/ALRubinger/aileron/internal/store/mem"
	"github.com/ALRubinger/aileron/internal/store/postgres"
	"github.com/ALRubinger/aileron/internal/vault"
	"github.com/ALRubinger/aileron/internal/version"
	"github.com/google/uuid"
)

// Config customizes [NewHandlerWithConfig]. The zero value is valid:
// every field is optional and defaults reproduce the historic
// [NewHandler] behaviour.
type Config struct {
	// Vault overrides the default in-memory random-KEK vault. Pass a
	// vault produced by [vault.Init] / [vault.Unlock] when the
	// runtime is being driven by `aileron launch`, so credentials
	// land in the user's encrypted file at ~/.aileron/secrets.json
	// rather than in a process-lifetime memory vault.
	Vault vault.Vault

	// WebappURL is the base URL the action-approval review surface is
	// reachable at (#418). The notification's ReviewURL is built as
	// `<WebappURL>/approvals?focus=<id>` so the terminal-printed block
	// and agent-facing approval message carry a per-approval deep link
	// the user can click or re-open via `aileron open approval <id>`.
	//
	// The daemon serves the webapp itself (see webapp_embed.go), so the
	// natural default is the daemon's own bound URL. Production wiring
	// (`internal/server`) sets that default after binding. This field
	// and the `AILERON_WEBAPP_URL` environment variable are overrides
	// for deployments where the daemon's bound URL is not what users
	// hit: reverse-proxied installs, `--bind 0.0.0.0:...` containers, or
	// a future split-origin webapp.
	//
	// Precedence: cfg.WebappURL (this field) → AILERON_WEBAPP_URL env →
	// empty. Empty surfaces a generic prompt instead of a URL — the
	// operator at least learns something needs attention.
	WebappURL string

	// LocalDaemonToken enables always-on bearer authentication for the
	// local daemon's /v1/* surface. CLI and in-container clients read
	// the token from daemon.json and send Authorization: Bearer <token>.
	// The local webapp obtains an HttpOnly same-origin cookie through
	// GET /v1/auth/handshake so EventSource can authenticate too.
	LocalDaemonToken string

	// CaveatSigningKey is the per-daemon HMAC key used to mint and
	// validate session-scoped caveat tokens (ADR-0024, #958). Generated
	// randomly at startup alongside LocalDaemonToken and never persisted.
	// When set, CreateSession returns a caveat token bound to the new
	// session, and localDaemonAuthMiddleware accepts that token for the
	// minimal route set the sandbox aileron-mcp needs. Empty disables
	// caveat minting/validation; only the master token is then accepted.
	CaveatSigningKey []byte

	// Notifier overrides the default notification dispatcher (log +
	// terminal multi). Production wiring leaves this nil; tests inject
	// a recorder to observe the action-approval notification payload
	// without writing to stderr. The injected notifier replaces the
	// default — when set, no log / terminal notifications fire from
	// this handler, only the supplied notifier.
	Notifier notify.Notifier

	// LocalVaultPath, when set, runs the daemon with a deferred-unlock
	// local file vault (#429): NewHandlerWithConfig wraps the supplied
	// Vault in a [vault.LockableVault] and marks vaultLocked = true.
	// Vault-needing endpoints return 423 Locked until the user POSTs
	// their passphrase to /v1/vault/unlock — typically via the webapp
	// passphrase modal.
	//
	// Distinct from Vault: when LocalVaultPath is set, Vault is the
	// initial (locked) inner — usually nil, so the LockableVault
	// returns ErrCredentialUnavailable on credential reads until
	// unlock. Setting Vault to a pre-unlocked vault while also
	// setting LocalVaultPath is supported (`vaultLocked` stays false)
	// for tests that want to drive the UnlockLocalVault handler
	// without provisioning a real vault file.
	LocalVaultPath string

	// ActionApprovals overrides the default in-process action-
	// approval queue. Production wiring under `aileron launch`
	// passes a shared queue here so the embedded gateway *and* the
	// CommsServer (which fields `aileron-mcp`'s send-shaped tool
	// calls per #428) register their pending entries on the same
	// queue — one webapp surface, one SSE stream, one decision path.
	// Nil falls back to a fresh queue, preserving the historic
	// behaviour for callers that don't need cross-component sharing.
	ActionApprovals *approval.ActionApprovalQueue

	// ActionApprovalPersister is the durable backing for the
	// action-approval queue (#649). Wired alongside the queue itself;
	// every state transition is appended so a daemon restart can
	// resume in-flight approvals. The daemon constructs a JSONL-
	// backed store pointed at ~/.aileron/approvals.jsonl and passes
	// it here. Nil keeps the queue purely in-memory (tests / cloud-
	// shaped daemons).
	ActionApprovalPersister approval.Persister

	// LoadedActionApprovals seeds the queue from a previously-
	// persisted snapshot at startup. Records arrive in registration
	// order; entries left in [approval.OutcomeRunning] at replay
	// have already been reaped by the wiring layer (mapped to
	// [approval.OutcomeFailed] with a reconciliation reason) before
	// they are passed here, so the queue can load them verbatim.
	// Non-terminal entries get respawned executor goroutines after
	// the queue is fully wired. Nil / empty disables replay.
	LoadedActionApprovals []approval.PersistedRecord

	// Sessions is the persistent store for `aileron launch` session
	// records (ADR-0012). The daemon constructs a JSONL-backed store
	// pointed at ~/.aileron/sessions.jsonl and passes it here. Nil
	// disables the /v1/sessions endpoints — they return 503 — which
	// is the right behavior for cloud-shaped deployments and tests
	// that don't exercise the launch session surface.
	Sessions sessions.Store

	// NotifyQueue is the daemon-wide queue of incoming channel
	// messages (ADR-0012, step 9B-2). Populated by listener goroutines
	// after [OnVaultUnlock] resolves credentials; consumed by the
	// `/v1/sessions/{id}/comms/messages` endpoint that powers
	// `aileron-mcp`'s `read_messages` tool. Nil disables every comms
	// endpoint.
	NotifyQueue *comms.NotifyQueue

	// Listeners is the registry of active channel listeners keyed
	// by service name. Populated by the vault-unlock callback;
	// consumed by `/comms/send` and `/comms/draft` to dispatch
	// outbound messages after the user approves. Pair with
	// [NotifyQueue] — both nil or both set.
	Listeners *comms.ListenerRegistry

	// OnVaultUnlock fires after a successful POST /v1/vault/unlock,
	// stamped with the freshly-unlocked vault. The daemon registers
	// a callback here when channel listeners need credentials
	// resolved at unlock time. Nil is fine for tests / cloud
	// daemons that have nothing to do on unlock.
	//
	// Synchronous from the perspective of the unlock handler — the
	// HTTP response holds open until the callback returns. Listener
	// startup itself is fire-and-forget inside the callback so the
	// handler never blocks on remote handshakes.
	OnVaultUnlock func(vault.Vault)

	// AuditStateDir scopes the daily-rotated `audit-YYYY-MM-DD.jsonl`
	// files that `message_received` events land in. Empty disables
	// audit emission for inbound messages. Production passes
	// ~/.aileron; tests pass t.TempDir().
	AuditStateDir string

	// SandboxProxyStateDir is the daemon state directory that contains
	// session-scoped sandbox proxy artifacts generated by launch:
	// sessions/<session-id>/sandbox-proxy/{ca.pem,ca.key}. Empty keeps
	// transparent CONNECT proxy transport disabled.
	SandboxProxyStateDir string
}

// buildApprovalsReviewURL composes the approval-notification ReviewURL
// from the two-tier configuration: cfg.WebappURL (set by launch to the
// embedded gateway's URL — production path) takes precedence over the
// AILERON_WEBAPP_URL environment variable (the standalone-server case
// where users may run the webapp dev server elsewhere). Both empty
// returns "", which the terminal notifier handles by falling back to a
// generic prompt — the user at least learns something needs attention
// even without a clickable target.
//
// When approvalID is non-empty, the URL carries a `?focus=<id>` query
// so the webapp's approvals page can scroll/highlight the specific
// entry. Empty approvalID returns the bare list URL.
//
// Trailing slashes on the base URL are tolerated and stripped so the
// resulting URL never has a doubled slash before /approvals.
func buildApprovalsReviewURL(cfgURL, envURL, approvalID string) string {
	base := cfgURL
	if base == "" {
		base = envURL
	}
	if base == "" {
		return ""
	}
	out := strings.TrimRight(base, "/") + "/approvals"
	if approvalID != "" {
		out = out + "?focus=" + url.QueryEscape(approvalID)
	}
	return out
}

// newVaultUnlockedCh returns the channel respawned approval executors
// park on while waiting for the vault to be unlocked (#649). When the
// daemon starts already-unlocked the channel is returned pre-closed
// so a receive returns immediately; when it starts locked the channel
// stays open until [apiServer.UnlockLocalVault] closes it on a
// successful unlock.
func newVaultUnlockedCh(startVaultLocked bool) chan struct{} {
	ch := make(chan struct{})
	if !startVaultLocked {
		close(ch)
	}
	return ch
}

// daemonUnitLayers resolves the image-derived CLI-unit layers (#1322) for the
// daemon's default sandbox image. It reads the image's devcontainer.metadata
// label and projects each Feature's customizations.aileron.cli unit into a
// capture-descriptor layer and a proxybinding-entry layer, additive to the
// embedded defaults.
//
// The daemon binding table is built once at startup and is image-independent
// today, so the layer is resolved for the daemon's default sandbox base image.
// When the image is not present locally or carries no label the read returns
// "" and the layer is empty, a clean no-op preserving today's passthrough. A
// present-but-malformed unit is a loud error so a broken Feature fails startup
// rather than silently shipping nothing.
//
// It is a package var so a test can substitute a fake without driving a real
// container runtime; the production implementation resolves the default base
// image and the default runner.
var daemonUnitLayers = func(ctx context.Context) ([]capture.CaptureDescriptor, []proxybinding.Entry, error) {
	image := sandboxcomposition.BaseImage(version.Version)
	return unitloader.LayersFromImage(ctx, sandboxcontainer.DefaultRunner(), sandboxcontainer.DefaultRuntime, image)
}

// assembleHostBindings builds the daemon's host->credential binding table:
// the embedded built-in defaults, the image-derived unit layer (#1322), and
// the user override layer, merged at built-in < unit-derived < user
// precedence and adapted to the canonical binding table. The unit layer is
// purely additive; an image whose label cannot be read contributes nothing
// and preserves today's defaults-only table. A present-but-malformed unit, or
// a malformed descriptor in any layer, fails construction loudly rather than
// silently shipping an empty (passthrough) table.
func assembleHostBindings(ctx context.Context) (binding.HostBindings, error) {
	_, sealingLayer, err := daemonUnitLayers(ctx)
	if err != nil {
		return nil, fmt.Errorf("assemble host bindings: load image unit layer: %w", err)
	}
	opts := proxybinding.DefaultLoadOptions()
	opts.ExtraEntries = sealingLayer
	table, err := proxybinding.LoadHostBindings(opts)
	if err != nil {
		return nil, fmt.Errorf("assemble host bindings: %w", err)
	}
	return table, nil
}

// NewHandlerWithConfig is the configurable entry point. The launcher
// uses this with a passphrase-unlocked Vault; the standalone server
// binary uses NewHandler (dev-mode fallback).
func NewHandlerWithConfig(log *slog.Logger, cfg Config) (http.Handler, error) {
	ctx := context.Background()

	// --- OpenTelemetry bootstrap (issue #390 Phase 7 foundation) ---
	// Installs the global tracer provider and W3C TraceContext
	// propagator. Off by default; opt in with AILERON_OTEL_ENABLED.
	// The propagator is registered unconditionally so an inbound
	// `traceparent` is parsed even when this process emits no spans.
	// Span emission at action / connector / capability / approval
	// boundaries lands in follow-up PRs (gated on the audit-log file
	// rotation work — both surfaces will share a rotating writer so
	// spans and audit events can land on disk with the same retention
	// story).
	obsCfg, err := config.LoadObservabilityConfig()
	if err != nil {
		return nil, fmt.Errorf("observability config: %w", err)
	}
	_ = observability.Init(obsCfg, log)

	// --- In-memory stores ---
	intentStore := mem.NewIntentStore()
	approvalStore := mem.NewApprovalStore()
	policyStore := mem.NewPolicyStore()
	grantStore := mem.NewGrantStore()
	executionStore := mem.NewExecutionStore()
	connectorStore := mem.NewConnectorStore()
	credentialStore := mem.NewCredentialStore()
	fundingSourceStore := mem.NewFundingSourceStore()
	traceStore := mem.NewTraceStore()
	connectedAccountStore := mem.NewConnectedAccountStore()
	draftStore := mem.NewDraftStore()
	instructionStore := mem.NewUserInstructionStore()
	feedbackStore := mem.NewDraftFeedbackStore()
	llmConfigStore := mem.NewLLMConfigStore()

	// --- Connector registry ---
	// Per ADR-0002, the runtime ships only the registry — service-specific
	// connectors (Gmail, Stripe, GitHub, ...) live in sandboxed binaries
	// installed at FQNs like `github://aileron/gmail` and reach the
	// registry via `aileron action add`. The registry stays empty at
	// process start; entries are registered as connectors are installed
	// and loaded by the cstore install pipeline.
	registry := connector.NewRegistry()

	// --- Vault ---
	// Local daemons MUST supply either Vault (pre-unlocked) or
	// LocalVaultPath (deferred unlock via /v1/vault/unlock) — see
	// [Config.Vault] and [Config.LocalVaultPath]. Per #492 item 6a the
	// implicit dev-mode in-memory vault was removed: it silently dropped
	// credentials at every restart, and made /v1/vault/unlock dead code
	// in production (no inner vault to swap in via [vault.LockableVault]).
	//
	// Cloud-shaped daemons (AILERON_DATABASE_URL set) leave both fields
	// empty; the postgres vault wires up further down (search for
	// vault.NewPostgresVault). The check here just refuses to start a
	// local daemon without a vault — cloud is unaffected.
	//
	// Tests that previously got the dev fallback by passing Config{}
	// should construct an explicit [vault.NewMemVault] (wrap in
	// [vault.NewEncryptedVault] if encryption-at-rest is desired).
	v := cfg.Vault
	var lockableVault *vault.LockableVault
	startVaultLocked := false
	if cfg.LocalVaultPath != "" {
		lockableVault = vault.NewLockableVault()
		if v != nil {
			lockableVault.Set(v)
		} else {
			startVaultLocked = true
		}
		v = lockableVault
	} else if v == nil {
		if os.Getenv("AILERON_DATABASE_URL") == "" {
			return nil, fmt.Errorf("app: no vault configured: set Config.Vault or Config.LocalVaultPath")
		}
		// Cloud-shaped daemon: the postgres-backed vault is wired
		// further down (search vault.NewPostgresVault). The
		// placeholder MemVault here keeps early consumers (bindingStore
		// construction, GITHUB_TOKEN seed) from deref'ing nil. Matches
		// pre-#492 cloud behavior, where the dev-fallback memory vault
		// was likewise transiently held by those consumers before the
		// postgres reassignment overwrote `v`.
		v = vault.NewMemVault()
	}
	if token := os.Getenv("GITHUB_TOKEN"); token != "" && !startVaultLocked {
		v.Put(ctx, "connectors/github/default", []byte(token), vault.Metadata{
			Type: "api_key",
			Labels: map[string]string{
				"connector": "github",
			},
		})
		log.Info("seeded GitHub token into vault")
	}

	// --- Policy engine ---
	policyEngine := policy.NewRuleEngine(policyStore)

	if err := policy.SeedPolicies(ctx, policyStore); err != nil {
		return nil, err
	}
	log.Info("seeded default policies")

	// --- Approval orchestrator ---
	idGen := func() string { return uuid.New().String() }
	orchestrator := approval.NewInMemoryOrchestrator(approvalStore, idGen)

	// --- Notifier ---
	// Multi-notifier: log first (machine-readable, lands in
	// daemon.log), terminal second (human-readable block on stderr —
	// the daemon's stderr is redirected into daemon.log too, so a
	// `tail -f ~/.aileron/daemon.log` surfaces the formatted block
	// with the review URL and the `aileron open approval <id>` hint).
	//
	// The terminal notifier replaces an earlier desktop notifier
	// (osascript / notify-send) that fired native OS notifications
	// with no working click action on macOS — clicks resolved to
	// Finder, which was strictly worse than no notification at all.
	//
	// Tests inject a recorder via cfg.Notifier; that replaces the
	// default — no stderr writes happen from a test process.
	var notifier notify.Notifier = notify.NewMulti(
		notify.NewLogNotifier(log),
		notify.NewTerminalNotifier(os.Stderr, log),
	)
	if cfg.Notifier != nil {
		notifier = cfg.Notifier
	}

	// --- LLM gateway (dual-protocol: OpenAI + Anthropic) ---
	gatewayCfg, err := config.LoadGatewayConfig()
	if err != nil {
		return nil, fmt.Errorf("gateway config: %w", err)
	}
	openAIProxy := newGatewayProxy(gatewayCfg.OpenAIBaseURL, "openai", log)
	anthropicProxy := newGatewayProxy(gatewayCfg.AnthropicBaseURL, "anthropic", log)

	// --- HTTP handler ---
	mux := http.NewServeMux()

	// --- Source connectors (read-only context retrieval) ---
	sourceReg := source.NewRegistry()

	server := &apiServer{
		log:                  log,
		registry:             registry,
		policyEngine:         policyEngine,
		orchestrator:         orchestrator,
		vault:                v,
		lockableVault:        lockableVault,
		localVaultPath:       cfg.LocalVaultPath,
		vaultLocked:          startVaultLocked,
		vaultUnlockedCh:      newVaultUnlockedCh(startVaultLocked),
		notifier:             notifier,
		intents:              intentStore,
		approvals:            approvalStore,
		policies:             policyStore,
		grants:               grantStore,
		executions:           executionStore,
		connectors:           connectorStore,
		connectedAccounts:    connectedAccountStore,
		drafts:               draftStore,
		instructions:         instructionStore,
		feedback:             feedbackStore,
		credentials:          credentialStore,
		fundingSources:       fundingSourceStore,
		llmConfigs:           llmConfigStore,
		traces:               traceStore,
		sourceRegistry:       sourceReg,
		openAIProxy:          openAIProxy,
		anthropicProxy:       anthropicProxy,
		localDaemonToken:     cfg.LocalDaemonToken,
		caveatIssuer:         newCaveatIssuer(cfg.CaveatSigningKey),
		sandboxProxyStateDir: cfg.SandboxProxyStateDir,
		newID:                idGen,
		actions:              action.NewStore(action.DefaultDir()),
		actionState:          newActionStateStore(log),
		installer:            newConnectorInstaller(log),
		versionLister:        cstore.DefaultVersionLister(),
		hub:                  newHubClient(),
	}
	if res, err := server.actions.Load(); err != nil {
		log.Warn("failed to load actions directory", "dir", server.actions.Dir(), "error", err)
	} else if res.HasErrors() {
		for _, e := range res.Errors {
			log.Warn("action manifest failed to load", "file", e.File, "line", e.Line, "class", e.Class, "message", e.Message)
		}
	}

	// --- Audit log (ADR-0010) ---
	// File-backed JSONL by default so events survive daemon restart;
	// fall back to the in-memory store when no path is configured (the
	// AILERON_AUDIT_PATH env var explicitly set to empty) or when the
	// file path can't be opened. Postgres persistence is post-MVP. The
	// recorder mints audit IDs and stamps them onto failures so the
	// agent's response carries a working back-reference into the audit
	// log. Built before the sandbox executor and binding store so each
	// can emit lifecycle events through the same recorder.
	auditStore := newAuditStore(log)
	recorder := audit.NewRecorder(auditStore, nil, nil)
	server.auditStore = auditStore
	server.auditRecorder = recorder

	// --- Binding store (ADR-0006) ---
	// The vault is also the binding store: each binding's name is its
	// vault path. Listing binding metadata does not require the vault
	// to be unlocked; resolving a credential at execution time does.
	bindingStore := &binding.VaultStore{Vault: v}
	server.bindings = bindingStore
	server.oauth2Sessions = newOAuth2Sessions()

	// --- Host->credential binding table (#1193) ---
	// The user-level host-binding table is consulted at the TLS
	// forward-proxy boundary when no connector spec matches a decrypted
	// request (ADR-0019 launch passthrough). Every binding is loaded from
	// the declarative descriptors (#1197) through the generic two-layer
	// loader (built-in profiles -> user). This is the "config, not code"
	// path: a third-party CLI (e.g. Linear, api.linear.app via
	// header-template) is sealed by shipping a descriptor, with zero
	// per-CLI proxy code.
	//
	// GitHub is no longer special-cased in Go (#1248), and as of #1323 it is
	// no longer a central default either: its two bindings (github.com ->
	// basic, inject; api.github.com -> bearer, sentinel-swap) moved out of
	// internal/proxybinding/defaults/github.yaml into gh's devcontainer
	// Feature CLI unit, and the host fans them in through the image-derived
	// unit layer below. The remaining central defaults (e.g. Linear) are
	// still picked up by the embedded defaults glob; a user descriptor can
	// override any host, so each is a normal layer in the
	// built-in < unit-derived < user precedence, not privileged Go.
	//
	// The agent never holds these secrets; the daemon injects them at
	// egress. With no matching vault entry a binding still matches but
	// resolution fails closed (no secret, no header), so an
	// unauthenticated request behaves exactly as today's passthrough. A
	// malformed descriptor (fail-open we reject) surfaces here rather than
	// silently shipping an empty (passthrough) table.
	//
	// The image-derived unit layer (#1322) is additive: each CLI Feature in
	// the daemon's default sandbox image carries its sealing as data in the
	// image's devcontainer.metadata label, fanned out here between the
	// built-in defaults and the user layer (built-in < unit-derived < user).
	// An image whose label cannot be read (absent locally, no label) is a
	// clean no-op that preserves today's defaults-only table. A
	// present-but-malformed unit fails construction loudly, matching the
	// malformed-descriptor posture above. gh's unit is the sole source of its
	// bindings now that #1323 removed the central github.yaml; its projection
	// is byte-identical to that former default (pinned by internal/app's
	// TestGHUnitDriftGuard).
	server.hostBindings, err = assembleHostBindings(ctx)
	if err != nil {
		return nil, err
	}

	// --- Scope-drift detection (#726) ---
	// Hook fires after every successful connector install. If the
	// installed manifest demands an OAuth scope the binding's recorded
	// grant doesn't have, the binding is marked stale so the webapp /
	// CLI prompts the user to reauthorize.
	if server.installer != nil {
		server.installer.ScopeDriftHook = makeScopeDriftHook(bindingStore, log)
	}

	// --- Sandbox runtime (ADR-0005) ---
	// Per-call WASM instantiation. Falls back to the stub executor
	// when the sandbox runtime fails to come up — startup must not
	// fail just because Wazero couldn't initialize on this host.
	var executor action.Executor = action.StubExecutor{}
	sandboxRT, sandboxErr := sandbox.NewWazeroRuntime(ctx, sandbox.WithLogger(log))
	if sandboxErr != nil {
		log.Warn("sandbox runtime unavailable; using stub executor", "error", sandboxErr)
	} else {
		sandboxExec := action.NewSandboxExecutor(server.actions, server.installer.Store, sandboxRT, bindingStore)
		executor = sandboxExec
		server.sandboxRuntime = sandboxRT
	}
	server.executor = executor

	// --- Action-level approval queue (#418) ---
	// Surfaced to webapp/CLI via /v1/action-approvals; consumed by
	// RunAction when an action's manifest declares
	// [approval] required = true. Distinct from the rich governance
	// orchestrator above; converges with it post-MVP.
	//
	// Persistence (#649): when the wiring layer supplies a
	// [Persister] and a [LoadedActionApprovals] slice, the queue is
	// installed with the JSONL-backed store before any state
	// transitions can fire, then seeded with the prior process's
	// records. Non-terminal entries get respawned executor
	// goroutines further down (after the executor field is set).
	if cfg.ActionApprovals != nil {
		server.actionApprovals = cfg.ActionApprovals
	} else {
		server.actionApprovals = approval.NewActionApprovalQueue(nil, nil)
	}
	if cfg.ActionApprovalPersister != nil {
		server.actionApprovals.SetPersister(cfg.ActionApprovalPersister)
	}
	if len(cfg.LoadedActionApprovals) > 0 {
		server.actionApprovals.LoadRecords(cfg.LoadedActionApprovals)
	}

	// Sessions store (ADR-0012). Optional — nil leaves the
	// /v1/sessions endpoints to return 503. The daemon constructs a
	// JSONL-backed store and passes it via cfg; tests may pass an
	// in-memory implementation or omit entirely.
	server.sessions = cfg.Sessions
	server.actionApprovalTTL = 5 * time.Minute

	// Comms wiring (ADR-0012 step 9B-2). All four fields are
	// optional and travel together: nil queue or nil registry
	// makes every /comms/* endpoint return 503. The daemon
	// (cmd/server) constructs them; tests opt in selectively.
	server.notifyQueue = cfg.NotifyQueue
	server.listeners = cfg.Listeners
	server.onVaultUnlock = cfg.OnVaultUnlock
	server.auditStateDir = cfg.AuditStateDir
	// Wire the audit recorder so approval.requested / approval.approved
	// / approval.denied land in the audit log. Before this, the
	// approval flow left no audit trail — the user could grant or
	// deny consequential actions and a later "what did I approve
	// today?" lookup would come back empty.
	server.actionApprovals.SetAuditRecorder(recorder)

	// On Register, fire a notification so the user knows the agent is
	// blocked. The notification's ReviewURL points at the webapp's
	// /approvals page anchored at this approval via `?focus=<id>`;
	// precedence for the base URL is cfg.WebappURL (set by launch to
	// the embedded gateway's URL) → AILERON_WEBAPP_URL env var (for
	// the standalone-server case where users may run the webapp dev
	// server elsewhere) → empty, which falls back to a generic prompt
	// inside the terminal notifier so the user at least learns
	// something needs attention.
	webappURL := cfg.WebappURL
	if webappURL == "" {
		webappURL = os.Getenv("AILERON_WEBAPP_URL")
	}
	server.webappURL = webappURL
	envWebappURL := os.Getenv("AILERON_WEBAPP_URL")
	server.actionApprovals.SetOnRegister(func(a *approval.ActionApproval) {
		summary := a.ActionName
		if a.ConnectorFQN != "" {
			summary = a.ActionName + " on " + a.ConnectorFQN
		}
		_ = notifier.Notify(context.Background(), notify.Notification{
			ApprovalID: a.ID,
			Summary:    summary,
			ReviewURL:  buildApprovalsReviewURL(cfg.WebappURL, envWebappURL, a.ID),
		})
	})

	// Respawn executor goroutines for non-terminal entries the JSONL
	// store loaded into the queue (#649). Run AFTER the executor and
	// the audit recorder are wired so the goroutines have everything
	// they need; non-terminal entries listed by [NonTerminalLoaded]
	// are pending_approval, approved_not_started, or awaiting_vault.
	//
	// For each entry: look up the action manifest. If it's missing
	// (user uninstalled it between restarts), mark the entry failed
	// — the executor would crash on a Get error anyway, and the
	// failure path keeps the agent's /result poll honest. Otherwise
	// spawn the goroutine; for entries past Decide the queue has
	// pre-filled the decision channel so the goroutine resumes
	// directly into the vault-unlock wait + SetRunning + Execute.
	if len(cfg.LoadedActionApprovals) > 0 {
		for _, id := range server.actionApprovals.NonTerminalLoaded() {
			entry, ok := server.actionApprovals.Get(id)
			if !ok {
				continue
			}
			if entry.Kind != "" && entry.Kind != approval.ApprovalKindAction {
				// Comms-kind respawn is out of scope (#649 Non-goals)
				// — those entries replay as visible in /list but the
				// daemon won't auto-execute on the user's decision.
				log.Warn("approval kind not respawned",
					"approval_id", entry.ID, "kind", entry.Kind)
				continue
			}
			loaded, err := server.actions.Get(entry.ActionName)
			if err != nil {
				log.Warn("respawn: action manifest missing; marking approval failed",
					"approval_id", entry.ID,
					"action", entry.ActionName,
					"error", err)
				_ = server.actionApprovals.SetFailed(entry.ID, nil,
					"action manifest no longer exists; cannot resume after daemon restart")
				continue
			}
			connFQN := entry.ConnectorFQN
			if connFQN == "" && len(loaded.Manifest.Execute) > 0 {
				connFQN = loaded.Manifest.Execute[0].Connector
			}
			go server.executeApprovedAction(entry, entry.ActionName, connFQN, entry.Args, entry.SessionID)
		}
	}

	mux.HandleFunc("GET /v1/auth/handshake", server.handleLocalAuthHandshake)
	api.HandlerFromMux(server, mux)

	// Source connector tools API (read-only context retrieval).
	mux.HandleFunc("GET /v1/tools", server.handleListTools)
	mux.HandleFunc("POST /v1/tools/execute", server.handleExecuteTool)

	// User instructions API (context store envelope).
	mux.HandleFunc("POST /v1/instructions", server.handleCreateInstruction)
	mux.HandleFunc("GET /v1/instructions", server.handleListInstructions)
	mux.HandleFunc("GET /v1/instructions/", server.handleInstructionByID)
	mux.HandleFunc("PATCH /v1/instructions/", server.handleInstructionByID)
	mux.HandleFunc("DELETE /v1/instructions/", server.handleInstructionByID)

	// Draft feedback API (context store envelope).
	mux.HandleFunc("POST /v1/feedback", server.handleCreateFeedback)
	mux.HandleFunc("GET /v1/feedback", server.handleListFeedback)

	// LLM provider configuration (per-user).
	mux.HandleFunc("PUT /v1/llm-config", server.handleUpsertUserLLMConfig)
	mux.HandleFunc("GET /v1/llm-config", server.handleGetUserLLMConfig)
	mux.HandleFunc("DELETE /v1/llm-config", server.handleDeleteUserLLMConfig)
	// LLM provider configuration (per-enterprise, admin only).
	mux.HandleFunc("PUT /v1/enterprises/{id}/llm-config", server.handleUpsertEnterpriseLLMConfig)
	mux.HandleFunc("GET /v1/enterprises/{id}/llm-config", server.handleGetEnterpriseLLMConfig)
	mux.HandleFunc("DELETE /v1/enterprises/{id}/llm-config", server.handleDeleteEnterpriseLLMConfig)

	// Connected accounts — programmatic create (in addition to OAuth flow).
	mux.HandleFunc("POST /v1/connected-accounts", server.handleCreateConnectedAccount)

	// Draft lifecycle API.
	mux.HandleFunc("POST /v1/drafts", server.handleCreateDraft)
	mux.HandleFunc("GET /v1/drafts", server.handleListDrafts)
	mux.HandleFunc("GET /v1/drafts/", server.handleGetDraft)
	mux.HandleFunc("POST /v1/drafts/", server.handleDraftAction)

	registerDocsRoutes(mux)

	// Local webapp (#418): mount the embedded static build at `/`
	// as a catch-all. ServeMux gives more-specific paths their
	// handlers first, so every `/v1/*`, `/docs`, `/openapi.yaml`,
	// and `/auth/*` route registered above takes precedence — the
	// webapp handler only sees paths none of those claimed. Must
	// be the LAST registration on `mux`; subsequent specific
	// routes added in front of this one would still win, but
	// keeping the catch-all at the end matches the conventional
	// readability cue ("everything not handled above falls through
	// to the webapp"). See internal/app/webapp_handler.go.
	mux.Handle("/", webappHandler())

	// --- Auth (optional — enabled when AILERON_DATABASE_URL is set) ---
	authCfg, err := config.LoadAuthConfig()
	if err != nil {
		return nil, err
	}

	// Middleware chain: CORS -> request ID -> logging -> [auth] -> routes.
	var handler http.Handler = mux
	handler = localDaemonAuthMiddleware(cfg.LocalDaemonToken, newCaveatIssuer(cfg.CaveatSigningKey), handler)

	if authCfg.AuthEnabled() {
		db, err := postgres.NewDB(ctx, authCfg.DatabaseURL)
		if err != nil {
			return nil, err
		}
		log.Info("connected to PostgreSQL")

		enterpriseStore := postgres.NewEnterpriseStore(db)
		userStore := postgres.NewUserStore(db)
		userAuthProviderStore := postgres.NewUserAuthProviderStore(db)
		sessionStore := postgres.NewSessionStore(db)
		verificationCodeStore := postgres.NewVerificationCodeStore(db)

		userKeyMaterialStore := postgres.NewUserKeyMaterialStore(db)

		// Wire stores into apiServer for /me endpoints.
		server.enterprises = enterpriseStore
		server.users = userStore
		server.userAuthProviders = userAuthProviderStore
		server.userKeyMaterials = userKeyMaterialStore
		server.uiBaseURL = authCfg.UIBaseURL

		// KEK session cache for Phase 2 encrypted-at-rest vault.
		kekCache := auth.NewKEKSessionCache(authCfg.KEKSessionTTL())
		server.kekSessionCache = kekCache
		go func() {
			ticker := time.NewTicker(5 * time.Minute)
			defer ticker.Stop()
			for range ticker.C {
				kekCache.EvictExpired()
			}
		}()
		// Switch to Postgres-backed stores and vault now that the database is available.
		pgConnectedAccountStore := postgres.NewConnectedAccountStore(db)
		server.connectedAccounts = pgConnectedAccountStore
		server.drafts = postgres.NewDraftStore(db)
		pgInstructionStore := postgres.NewUserInstructionStore(db)
		server.instructions = pgInstructionStore
		server.feedback = postgres.NewDraftFeedbackStore(db)
		v = vault.NewPostgresVault(db.Pool)
		server.vault = v
		log.Info("using Postgres-backed stores and vault")

		tokenIssuer := auth.NewTokenIssuer(
			[]byte(authCfg.JWTSigningKey),
			authCfg.JWTIssuer,
			authCfg.AccessTokenTTL,
		)

		authRegistry := auth.NewRegistry()
		accountRegistry := account.NewRegistry()

		registerProviders(authCfg, authRegistry, accountRegistry, sourceReg, pgConnectedAccountStore, v, log)

		if len(accountRegistry.Providers()) > 0 {
			server.accountService = accountRegistry
		}

		// Per-user/per-org LLM configuration store.
		pgLLMConfigStore := postgres.NewLLMConfigStore(db)
		server.llmConfigs = pgLLMConfigStore

		// LLM-powered draft generation pipeline.
		if authCfg.LLMEnabled() {
			prompts := draft.LoadPrompts()
			server.draftPipeline = buildDraftPipeline(draftPipelineDeps{
				apiKey:         authCfg.AnthropicAPIKey,
				modelResearch:  authCfg.LLMModelResearch,
				modelSynthesis: authCfg.LLMModelSynthesis,
				llmConfigs:     pgLLMConfigStore,
				users:          userStore,
				accounts:       pgConnectedAccountStore,
				instructions:   pgInstructionStore,
				vault:          v,
				sourceReg:      sourceReg,
				log:            log,
				prompts:        prompts,
			})
			log.Info("enabled cloud draft generation",
				"research_model", authCfg.LLMModelResearch,
				"synthesis_model", authCfg.LLMModelSynthesis,
				"research_prompt_length", len(prompts.Research),
				"ghostwrite_prompt_length", len(prompts.Ghostwrite),
				"prompt_source", draft.PromptSource())
		}

		enforcer := auth.NewStoreEnforcer(enterpriseStore)

		mailer := newMailer(log, authCfg)

		authHandler := auth.NewHandler(auth.HandlerConfig{
			Log:               log,
			Registry:          authRegistry,
			Enforcer:          enforcer,
			Issuer:            tokenIssuer,
			Users:             userStore,
			UserAuthProviders: userAuthProviderStore,
			Enterprises:       enterpriseStore,
			Sessions:          sessionStore,
			VerificationCodes: verificationCodeStore,
			Mailer:            mailer,
			NewID:             idGen,
			UIBaseURL:         authCfg.UIBaseURL,
			RefreshTTL:        authCfg.RefreshTokenTTL,
			AutoVerifyEmail:   authCfg.AutoVerifyEmail,
		})
		authHandler.RegisterRoutes(mux)

		skipPaths := map[string]bool{
			"/v1/health": true,
		}
		handler = auth.MiddlewareWithConfig(tokenIssuer, auth.MiddlewareConfig{
			SkipPaths: skipPaths,
			OptionalAuthPrefixes: []string{
				"/v1/connect/", // OAuth callbacks may be unauthenticated.
			},
		})(handler)
		log.Info("auth middleware enabled")
	}

	handler = server.sandboxForwardProxyMiddleware(handler)
	handler = loggingMiddleware(log, handler)
	// Tracing middleware sits just outside the route handlers (and
	// auth) so the server-root span captures the full handler-side
	// latency. It runs inside requestID and cors so the per-request
	// log fields the upper middlewares depend on still appear in
	// log output even when tracing is off (the no-op default).
	handler = observability.HTTPMiddleware(spanNameForRequest)(handler)
	handler = requestIDMiddleware(handler)
	handler = corsMiddleware(handler)

	return handler, nil
}

// spanNameForRequest shapes server-root span names for the tracing
// middleware. Static paths get "METHOD /path"; path-templated routes
// carrying IDs (`POST /v1/actions/{name}/run`) collapse back to the
// template form so trace tooling groups them correctly. The two
// gateway endpoints get domain-shaped names (`aileron.gateway.openai.chat`,
// `aileron.gateway.anthropic.messages`) so consumers can filter for
// "all LLM round-trips" without grepping URL paths — they're the
// secondary trace surface from issue #390 Phase 7.
func spanNameForRequest(r *http.Request) string {
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/actions/") && strings.HasSuffix(r.URL.Path, "/run"):
		return "POST /v1/actions/{name}/run"
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		return "aileron.gateway.openai.chat"
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
		return "aileron.gateway.anthropic.messages"
	}
	return r.Method + " " + r.URL.Path
}

// newMailer returns a ResendMailer when a Resend API key is configured,
// otherwise a LogMailer for development/CI.
func newMailer(log *slog.Logger, cfg *config.AuthConfig) auth.Mailer {
	if cfg.ResendEnabled() {
		log.Info("email delivery via Resend", "from", cfg.MailFrom)
		return auth.NewResendMailer(auth.ResendMailerConfig{
			APIKey: cfg.ResendAPIKey,
			From:   cfg.MailFrom,
		})
	}
	log.Warn("RESEND_API_KEY not set — verification codes will be printed to the log (dev mode)")
	return auth.NewLogMailer(log)
}

// registerProviders populates the auth, account, and source registries based
// on which OAuth providers are configured. Extracted from NewHandler so the
// wiring logic can be tested without a database or HTTP server.
func registerProviders(
	cfg *config.AuthConfig,
	authReg *auth.Registry,
	accountReg *account.Registry,
	sourceReg *source.Registry,
	accounts store.ConnectedAccountStore,
	v vault.Vault,
	log *slog.Logger,
) {
	if cfg.GoogleSigninEnabled() {
		authReg.Register(googleauth.New(
			cfg.GoogleSigninClientID,
			cfg.GoogleSigninClientSecret,
		))
		log.Info("registered Google sign-in OAuth provider")
	}

	if cfg.GoogleConnectorEnabled() {
		accountReg.Register(account.NewGoogleService(
			cfg.GoogleConnectorClientID,
			cfg.GoogleConnectorClientSecret,
			accounts,
			v,
		))
		sourceReg.Register(gmailsource.New(cfg.GoogleConnectorClientID, cfg.GoogleConnectorClientSecret))
		sourceReg.Register(calendarsource.New(cfg.GoogleConnectorClientID, cfg.GoogleConnectorClientSecret))
		log.Info("enabled Google connected accounts and source connectors (Gmail, Calendar)")
	}

	if cfg.GitHubSigninEnabled() {
		authReg.Register(githubauth.New(
			cfg.GitHubSigninClientID,
			cfg.GitHubSigninClientSecret,
		))
		log.Info("registered GitHub sign-in OAuth provider")
	}

	if cfg.GitHubConnectorEnabled() {
		accountReg.Register(account.NewGitHubAccountService(
			cfg.GitHubConnectorClientID,
			cfg.GitHubConnectorClientSecret,
			accounts,
			v,
		))
		sourceReg.Register(githubsource.New())
		log.Info("enabled GitHub connected accounts and source connector")
	}
}

// newConnectorInstaller wires the cstore install pipeline (ADR-0004) with
// the v1 production defaults: HTTP fetcher, default per-scheme resolver,
// and a ReloadingKeyring backed by `~/.aileron/keyring.json` (per the
// file-based scheme introduced for #366). A missing or empty file means
// no publishers are trusted, and the install pipeline fails closed on
// every call (per ADR-0004's failure-modes table). Users opt in to a
// publisher by adding its ed25519 public key to the keyring file via
// `aileron keyring trust`.
//
// The verifier reloads the file on every install request so out-of-band
// edits (the `aileron keyring trust` CLI writes the file directly,
// bypassing the daemon) take effect immediately without a daemon
// restart. Cost is one small JSON parse per install, which is dominated
// by the network fetch in the same pipeline.
//
// The store root is `~/.aileron/store` per ADR-0004 §"Content-addressed
// store layout"; tests substitute their own Installer on the apiServer
// directly so the running server isn't required to spin up a real store.
func newConnectorInstaller(log *slog.Logger) *cstore.Installer {
	store := cstore.NewStore(cstore.DefaultRoot())
	if err := store.LoadIndex(); err != nil {
		log.Warn("failed to load connector index; rebuilding from disk",
			"root", store.Root(), "error", err)
		_ = store.RebuildIndex()
	}
	return &cstore.Installer{
		Resolver: cstore.DefaultResolver(),
		Fetcher:  &cstore.HTTPFetcher{},
		Verifier: &cstore.ReloadingKeyring{Path: cstore.DefaultKeyringPath()},
		Store:    store,
	}
}

// makeScopeDriftHook builds the closure the installer fires after a
// successful connector install (#726). The hook compares each affected
// binding's recorded GrantedScopes against the manifest's current scope
// set and marks bindings stale when they're missing a required scope.
//
// Two flavors of "stale" are stamped:
//
//   - Bindings with no recorded grant (created before scope tracking
//     landed) are marked `no_grant_record` — we can't prove they
//     satisfy any scope set, so the user reauthorizes once to teach us.
//   - Bindings whose recorded grant lacks a required scope are marked
//     `scope_drift` with the missing scope list, so the webapp/CLI can
//     show the user exactly what reauthorization will unlock.
//
// Drift semantics are strict-superset on required: a manifest that
// drops a previously-required scope does NOT trigger stale, because the
// extra granted scope is benign. The reverse — manifest demands a
// scope the binding lacks — does.
//
// When the manifest declares no OAuth scopes (kind != "oauth2"), the
// hook still fires with a nil scope list so migration-marking can run
// on bindings whose connector now exposes OAuth where it didn't before.
// In that case bindings with no recorded grant are still marked.
//
// Errors inside the hook (vault locked, vault store unavailable) are
// logged at warn level and the install completes successfully —
// drift detection is best-effort and a locked vault legitimately has
// nothing to enumerate.
func makeScopeDriftHook(store binding.Store, log *slog.Logger) cstore.ScopeDriftHook {
	return func(ctx context.Context, fqn string, required []string) []cstore.StaleBindingTransition {
		if store == nil {
			return nil
		}
		all, err := store.List(ctx)
		if err != nil {
			log.Warn("scope-drift detection: list bindings failed",
				"connector_fqn", fqn, "error", err)
			return nil
		}
		var transitions []cstore.StaleBindingTransition
		for _, b := range all {
			if b.ConnectorFQN != fqn {
				continue
			}
			// Only OAuth bindings carry scope grants; api_key /
			// other kinds are nothing to check.
			if b.Kind != "oauth2" {
				continue
			}
			next := evaluateScopeDrift(b, required)
			if next == nil {
				continue
			}
			if err := store.UpdateMetadata(ctx, *next); err != nil {
				log.Warn("scope-drift detection: update binding failed",
					"binding", string(b.Name), "error", err)
				continue
			}
			// Only surface transitions *to* stale; a clear-stale flip
			// (manifest dropped a scope, binding is now satisfied) is
			// not something the operator needs to act on.
			if next.Status != binding.StatusStale {
				continue
			}
			transitions = append(transitions, cstore.StaleBindingTransition{
				Name:          string(next.Name),
				ConnectorFQN:  next.ConnectorFQN,
				StaleReason:   next.StaleReason,
				MissingScopes: next.MissingScopes,
				Service:       next.Service,
			})
		}
		return transitions
	}
}

// evaluateScopeDrift returns the binding (with updated Status /
// StaleReason / MissingScopes) when the binding's drift state should
// change, or nil when the binding is already in the desired state.
// Split from makeScopeDriftHook so the policy decision is pure and
// independently testable.
func evaluateScopeDrift(b binding.Binding, required []string) *binding.Binding {
	if len(b.GrantedScopes) == 0 {
		// Migration: bindings predating scope tracking. Already stale?
		// Leave alone. Otherwise flip to stale with no_grant_record so
		// the user reauthorizes once.
		if b.Status == binding.StatusStale && b.StaleReason == binding.StaleReasonNoGrantRecord {
			return nil
		}
		updated := b
		updated.Status = binding.StatusStale
		updated.StaleReason = binding.StaleReasonNoGrantRecord
		updated.MissingScopes = nil
		return &updated
	}
	missing := binding.MissingScopes(required, b.GrantedScopes)
	if len(missing) > 0 {
		// No-op when the recorded stale state already matches.
		if b.Status == binding.StatusStale &&
			b.StaleReason == binding.StaleReasonScopeDrift &&
			stringSliceEqual(b.MissingScopes, missing) {
			return nil
		}
		updated := b
		updated.Status = binding.StatusStale
		updated.StaleReason = binding.StaleReasonScopeDrift
		updated.MissingScopes = missing
		return &updated
	}
	// Manifest scopes are a subset of granted scopes — binding
	// satisfies the current manifest. Clear any stale state we set
	// earlier (e.g. a subsequent install dropped the scope demands).
	if b.Status == binding.StatusStale {
		updated := b
		updated.Status = binding.StatusActive
		updated.StaleReason = ""
		updated.MissingScopes = nil
		return &updated
	}
	return nil
}

// stringSliceEqual reports whether a and b have the same elements in
// the same order. Used by evaluateScopeDrift to short-circuit
// no-op metadata updates that would still cost a vault round-trip.
func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// newActionStateStore builds the per-action user-preference overlay backed
// by `~/.aileron/action-state.json`. Returns a nil store on read failure so
// the daemon keeps serving — disabled callers fall back to the default
// "all actions enabled" view and the user can recover by deleting the
// malformed file.
func newActionStateStore(log *slog.Logger) action.StateStore {
	s, err := action.NewFileStateStore(action.DefaultStatePath())
	if err != nil {
		log.Warn("failed to load action state overlay; treating all actions as enabled",
			"path", action.DefaultStatePath(), "error", err)
		return nil
	}
	return s
}

// newHubClient builds the Hub client (ADR-0013) using config from
// `~/.aileron/config.yaml`. A missing or empty config falls back to
// the public Aileron connector Hub. Tests construct apiServer with
// a different `hub *hub.Client` value pointing at a file:// fixture.
//
// Returns a fully-initialized client even on config load error — a
// broken `~/.aileron/config.yaml` shouldn't disable Hub functionality
// silently. The error path logs and proceeds with defaults.
func newHubClient() *hub.Client {
	cfg, _ := config.LoadAileronConfig(config.DefaultAileronConfigPath())
	url := config.DefaultHubURL
	if cfg != nil && cfg.Hub != nil && cfg.Hub.URL != "" {
		url = cfg.Hub.URL
	}
	return &hub.Client{URL: url}
}
