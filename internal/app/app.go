// Package app wires together the Aileron control plane components and exposes
// them as an http.Handler. It is imported by the standalone server binary.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/internal/account"
	"github.com/ALRubinger/aileron/internal/action"
	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/approval"
	"github.com/ALRubinger/aileron/internal/auth"
	githubauth "github.com/ALRubinger/aileron/internal/auth/github"
	googleauth "github.com/ALRubinger/aileron/internal/auth/google"
	"github.com/ALRubinger/aileron/internal/config"
	"github.com/ALRubinger/aileron/internal/connector"
	googlecalendar "github.com/ALRubinger/aileron/internal/connector/calendar/google"
	gmailconnector "github.com/ALRubinger/aileron/internal/connector/email/gmail"
	"github.com/ALRubinger/aileron/internal/connector/git/github"
	"github.com/ALRubinger/aileron/internal/connector/payments/stripe"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/draft"
	"github.com/ALRubinger/aileron/internal/enclave"
	"github.com/ALRubinger/aileron/internal/intercept"
	"github.com/ALRubinger/aileron/internal/sandbox"
	"github.com/ALRubinger/aileron/internal/notify"
	"github.com/ALRubinger/aileron/internal/observability"
	"github.com/ALRubinger/aileron/internal/policy"
	"github.com/ALRubinger/aileron/internal/source"
	calendarsource "github.com/ALRubinger/aileron/internal/source/calendar"
	githubsource "github.com/ALRubinger/aileron/internal/source/github"
	gmailsource "github.com/ALRubinger/aileron/internal/source/gmail"
	slacksource "github.com/ALRubinger/aileron/internal/source/slack"
	"github.com/ALRubinger/aileron/internal/store"
	"github.com/ALRubinger/aileron/internal/store/mem"
	"github.com/ALRubinger/aileron/internal/store/postgres"
	"github.com/ALRubinger/aileron/internal/vault"
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

	// DisableAugmentation, when true, skips the gateway's
	// tool-augmentation + intercept loop. Chat-completion and Messages
	// requests pass through unchanged to the upstream LLM. Used by
	// `aileron launch` when MCP is the canonical surface for action
	// discovery (per the working-session decision on 2026-05-03): the
	// gateway then provides only the action-discovery / -execution
	// HTTP endpoints (consumed by `aileron-mcp`) and serves as the
	// substrate for Phase 2 request mediation, without exposing
	// duplicate action tools to the LLM in-band.
	DisableAugmentation bool

	// WebappURL is the base URL of the Aileron webapp the user opens to
	// approve / deny gated actions (#418). When set, the action-approval
	// notification's ReviewURL is built as `<WebappURL>/approvals` so
	// the desktop notification carries a clickable target.
	//
	// Empty falls back to the AILERON_WEBAPP_URL environment variable.
	// Both empty means notifications fire with a generic "Open the
	// Aileron webapp to approve or deny" prompt instead of a URL —
	// the operator at least learns something needs attention.
	//
	// Launch sets this to the embedded gateway's own URL so the
	// notification points at the same daemon the agent is talking to.
	// Once the daemon serves `ui/build` directly (post-MVP cleanup),
	// that URL becomes the working webapp entry point automatically.
	// Until then, users running the webapp dev server elsewhere can
	// override via AILERON_WEBAPP_URL.
	WebappURL string

	// Notifier overrides the default notification dispatcher (log +
	// desktop multi). Production wiring leaves this nil; tests inject
	// a recorder to observe the action-approval notification payload
	// without firing OS notifications. The injected notifier replaces
	// the default — when set, no log / desktop notifications fire from
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
}

// NewHandler creates a fully-wired Aileron control plane HTTP handler
// with in-memory stores, seeded policies, and registered connectors.
// Equivalent to NewHandlerWithConfig(log, Config{}).
func NewHandler(log *slog.Logger) (http.Handler, error) {
	return NewHandlerWithConfig(log, Config{})
}

// buildApprovalsReviewURL composes the approval-notification ReviewURL
// from the two-tier configuration: cfg.WebappURL (set by launch to the
// embedded gateway's URL — production path) takes precedence over the
// AILERON_WEBAPP_URL environment variable (the standalone-server case
// where users may run the webapp dev server elsewhere). Both empty
// returns "", which the desktop notifier handles by falling back to a
// generic "Open the Aileron webapp" prompt — the user at least learns
// something needs attention even without a clickable target.
//
// Trailing slashes on the base URL are tolerated and stripped so the
// resulting URL never has a doubled slash before /approvals.
func buildApprovalsReviewURL(cfgURL, envURL string) string {
	base := cfgURL
	if base == "" {
		base = envURL
	}
	if base == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/approvals"
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
	registry := connector.NewRegistry()
	registry.Register(ctx, stripe.New())
	// Google Calendar connector — OAuth client credentials are loaded later
	// from auth config. Empty strings here mean token refresh won't work
	// until registerProviders re-registers with real credentials.
	registry.Register(ctx, googlecalendar.New("", ""))
	registry.Register(ctx, github.New())

	// --- Vault ---
	// When the caller (typically `aileron launch`) supplied a
	// passphrase-unlocked vault via Config, use it directly. Otherwise
	// fall back to the dev-mode in-memory vault with an
	// auto-generated KEK so the standalone server binary still works
	// without any setup. Either way, the on-the-wire path is encrypted
	// — there is no plaintext bypass.
	//
	// When cfg.LocalVaultPath is set, wrap the supplied (possibly nil)
	// vault in a LockableVault — the daemon starts vault-locked, every
	// component that holds a reference to v gets the wrapper, and
	// /v1/vault/unlock swaps the unlocked inner vault in (#429).
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
		var err error
		v, err = newLocalEncryptedVault()
		if err != nil {
			return nil, err
		}
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
	grantCapabilityKey := make([]byte, 32)
	if _, err := rand.Read(grantCapabilityKey); err != nil {
		return nil, fmt.Errorf("grant capability key: %w", err)
	}

	// --- Notifier ---
	// Multi-notifier: log first (always works), desktop second (best-
	// effort native OS notifications via osascript / notify-send). The
	// desktop notifier is what nudges the user toward the webapp when
	// an action-approval lands; the log notifier is the durable
	// backstop. Slack / email notifiers (the existing rich-governance
	// channels) compose into the same Multi when configured.
	//
	// Tests inject a recorder via cfg.Notifier; that replaces the
	// default — no OS notifications fire from a test process.
	var notifier notify.Notifier = notify.NewMulti(
		notify.NewLogNotifier(log),
		notify.NewDesktopNotifier(log),
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

	// --- TEE (optional — enabled when AILERON_TEE_PROVIDER is set) ---
	teeCfg := config.LoadTEEConfig()
	var enclaveClient enclave.Client
	var enclaveVerifier enclave.Verifier
	if teeCfg.TEEEnabled() {
		var teeErr error
		enclaveClient, enclaveVerifier, teeErr = newEnclaveClient(teeCfg, log, registry)
		if teeErr != nil {
			return nil, teeErr
		}
	}

	// --- HTTP handler ---
	mux := http.NewServeMux()

	// --- Source connectors (read-only context retrieval) ---
	sourceReg := source.NewRegistry()

	server := &apiServer{
		log:                log,
		registry:           registry,
		policyEngine:       policyEngine,
		orchestrator:       orchestrator,
		vault:              v,
		lockableVault:      lockableVault,
		localVaultPath:     cfg.LocalVaultPath,
		vaultLocked:        startVaultLocked,
		notifier:           notifier,
		intents:            intentStore,
		approvals:          approvalStore,
		policies:           policyStore,
		grants:             grantStore,
		executions:         executionStore,
		connectors:         connectorStore,
		connectedAccounts:  connectedAccountStore,
		drafts:             draftStore,
		instructions:       instructionStore,
		feedback:           feedbackStore,
		credentials:        credentialStore,
		fundingSources:     fundingSourceStore,
		llmConfigs:         llmConfigStore,
		traces:             traceStore,
		sourceRegistry:     sourceReg,
		enclaveClient:      enclaveClient,
		enclaveVerifier:    enclaveVerifier,
		teeCfg:             teeCfg,
		grantCapabilityKey: grantCapabilityKey,
		openAIProxy:        openAIProxy,
		anthropicProxy:     anthropicProxy,
		newID:              idGen,
		actions:            action.NewStore(action.DefaultDir()),
		installer:          newConnectorInstaller(log),
		versionLister:      cstore.DefaultVersionLister(),
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

	// --- Sandbox runtime (ADR-0005) ---
	// Per-call WASM instantiation. Falls back to the stub executor
	// when the sandbox runtime fails to come up — startup must not
	// fail just because Wazero couldn't initialize on this host.
	var executor action.Executor = action.StubExecutor{}
	sandboxRT, sandboxErr := sandbox.NewWazeroRuntime(ctx, sandbox.WithLogger(log))
	if sandboxErr != nil {
		log.Warn("sandbox runtime unavailable; using stub executor", "error", sandboxErr)
	} else {
		executor = action.NewSandboxExecutor(server.actions, server.installer.Store, sandboxRT, bindingStore)
		server.sandboxRuntime = sandboxRT
	}
	server.executor = executor

	// --- Action-level approval queue (#418) ---
	// In-memory; per-process. Surfaced to webapp/CLI via
	// /v1/action-approvals; consumed by RunAction when an action's
	// manifest declares [approval] required = true. Distinct from the
	// rich governance orchestrator above; converges with it post-MVP.
	if cfg.ActionApprovals != nil {
		server.actionApprovals = cfg.ActionApprovals
	} else {
		server.actionApprovals = approval.NewActionApprovalQueue(nil, nil)
	}
	server.actionApprovalTTL = 5 * time.Minute
	// Wire the audit recorder so approval.requested / approval.approved
	// / approval.denied land in the audit log. Before this, the
	// approval flow left no audit trail — the user could grant or
	// deny consequential actions and a later "what did I approve
	// today?" lookup would come back empty.
	server.actionApprovals.SetAuditRecorder(recorder)

	// On Register, fire a notification so the user knows the agent is
	// blocked. The notification's ReviewURL points at the webapp's
	// /approvals page; precedence is cfg.WebappURL (set by launch to
	// the embedded gateway's URL) → AILERON_WEBAPP_URL env var (for
	// the standalone-server case where users may run the webapp dev
	// server elsewhere) → empty, which falls back to a generic prompt
	// inside the desktop notifier so the user at least learns
	// something needs attention.
	webappURL := cfg.WebappURL
	if webappURL == "" {
		webappURL = os.Getenv("AILERON_WEBAPP_URL")
	}
	server.webappURL = webappURL
	reviewURL := buildApprovalsReviewURL(cfg.WebappURL, os.Getenv("AILERON_WEBAPP_URL"))
	server.actionApprovals.SetOnRegister(func(a *approval.ActionApproval) {
		summary := a.ActionName
		if a.ConnectorFQN != "" {
			summary = a.ActionName + " on " + a.ConnectorFQN
		}
		_ = notifier.Notify(context.Background(), notify.Notification{
			ApprovalID: a.ID,
			Summary:    summary,
			ReviewURL:  reviewURL,
		})
	})

	// --- Intercept engine (stage 4) ---
	// When augmentation is disabled (e.g. `aileron launch` with MCP as
	// the canonical action-exposure surface) the engine is not built;
	// the gateway handler degrades to a pass-through proxy and action
	// discovery / execution flow through `/v1/actions*` only.
	if !cfg.DisableAugmentation {
		engine, engineErr := intercept.New(intercept.Config{
			OpenAIUpstream:    gatewayCfg.OpenAIBaseURL,
			AnthropicUpstream: gatewayCfg.AnthropicBaseURL,
			Actions:           server.actions,
			Executor:          executor,
			Log:               log,
			Recorder:          recorder,
		})
		if engineErr != nil {
			return nil, fmt.Errorf("intercept engine: %w", engineErr)
		}
		server.interceptEngine = engine
	}

	if teeCfg.TEEEnabled() {
		server.teeState = newTeeState()
	}
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
		server.escrowTTL = authCfg.EscrowTTL()
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
		escrowIndexStore := postgres.NewEscrowIndexStore(db)
		server.escrowIndexStore = escrowIndexStore

		// Background cleanup of expired escrow index entries.
		go func() {
			ticker := time.NewTicker(10 * time.Minute)
			defer ticker.Stop()
			for range ticker.C {
				if n, err := escrowIndexStore.DeleteExpired(context.Background()); err == nil && n > 0 {
					log.Info("cleaned expired escrow index entries", "count", n)
				}
			}
		}()

		// Load persisted escrow index so async flows (Slack) work after restart.
		if idx, err := escrowIndexStore.LoadAll(ctx); err == nil {
			for path, id := range idx {
				server.escrowIndex.Store(path, id)
			}
			if len(idx) > 0 {
				log.Info("loaded escrow index from database", "entries", len(idx))
			}

			// Reconcile: prune index entries the enclave no longer has.
			if enclaveClient != nil && len(idx) > 0 {
				reconcileEscrowIndex(ctx, log, enclaveClient, &server.escrowIndex, escrowIndexStore, idx)
			}
		}
		log.Info("using Postgres-backed stores and vault")

		// System vault for infrastructure secrets (ADR-0020).
		if authCfg.SystemVaultEnabled() {
			sysKey, err := hex.DecodeString(authCfg.SystemVaultKey)
			if err != nil {
				return nil, fmt.Errorf("AILERON_SYSTEM_VAULT_KEY: invalid hex: %w", err)
			}
			sysVault, err := vault.NewEncryptedVault(
				vault.NewPostgresSystemVault(db.Pool),
				sysKey,
			)
			if err != nil {
				return nil, fmt.Errorf("system vault: %w", err)
			}
			server.systemVault = sysVault
			log.Info("enabled system vault for infrastructure secrets")
		}

		tokenIssuer := auth.NewTokenIssuer(
			[]byte(authCfg.JWTSigningKey),
			authCfg.JWTIssuer,
			authCfg.AccessTokenTTL,
		)

		authRegistry := auth.NewRegistry()
		accountRegistry := account.NewRegistry()

		registerProviders(authCfg, authRegistry, accountRegistry, sourceReg, registry, pgConnectedAccountStore, v, log)

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

		if authCfg.SlackEnabled() {
			server.slackClientID = authCfg.SlackClientID
			server.slackClientSecret = authCfg.SlackClientSecret
			server.slackSigningSecret = authCfg.SlackSigningSecret
			server.slackDedup = newSlackEventDedup()
			server.slackAgentClient = defaultSlackAgentClient{}
			mux.HandleFunc("POST /v1/webhooks/slack/events", server.handleSlackEvent)
			mux.HandleFunc("POST /v1/webhooks/slack/interactions", server.handleSlackInteraction)
			mux.HandleFunc("POST /v1/webhooks/slack/commands", server.handleSlackCommand)
			mux.HandleFunc("GET /v1/slack/install/callback", server.handleSlackInstall)
			log.Info("enabled Slack Events API webhook, interaction, command, and install endpoints")
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
			"/v1/health":                      true,
			"/v1/tee/status":                  true,
			"/v1/tee/jwks":                    true,
			"/v1/webhooks/slack/events":       true,
			"/v1/webhooks/slack/interactions": true,
			"/v1/webhooks/slack/commands":     true,
			"/v1/slack/install/callback":      true,
		}
		handler = auth.MiddlewareWithConfig(tokenIssuer, auth.MiddlewareConfig{
			SkipPaths: skipPaths,
			OptionalAuthPrefixes: []string{
				"/v1/connect/", // OAuth callbacks may be unauthenticated (e.g. Slack Marketplace installs)
			},
		})(handler)
		log.Info("auth middleware enabled")
	}

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
// middleware. Default-style "METHOD /path" is fine for static paths
// (`POST /v1/chat/completions`), but path-templated routes carrying
// IDs (`POST /v1/actions/{name}/run`) blow up cardinality if the
// raw URL is used as the span name. We collapse the two known
// templated routes back to their template form so trace tooling
// groups them correctly.
func spanNameForRequest(r *http.Request) string {
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/actions/") && strings.HasSuffix(r.URL.Path, "/run"):
		return "POST /v1/actions/{name}/run"
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		return "POST /v1/chat/completions"
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
		return "POST /v1/messages"
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
	connectorRegistry *connector.Registry,
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
		// Re-register execution connectors with OAuth credentials for token refresh.
		connectorRegistry.Register(context.Background(), googlecalendar.New(cfg.GoogleConnectorClientID, cfg.GoogleConnectorClientSecret))
		connectorRegistry.Register(context.Background(), gmailconnector.New(cfg.GoogleConnectorClientID, cfg.GoogleConnectorClientSecret))
		log.Info("enabled Google connected accounts, source connectors, and execution connectors (Gmail, Calendar)")
	}

	if cfg.SlackEnabled() {
		accountReg.Register(account.NewSlackService(
			cfg.SlackClientID,
			cfg.SlackClientSecret,
			accounts,
			v,
		))
		sourceReg.Register(slacksource.New())
		log.Info("enabled Slack connected accounts and source connector")
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
// and an empty Ed25519 keyring. The keyring is intentionally empty —
// signing-keys-and-rotation is deferred per ADR-0002. The verifier is
// loaded from `~/.aileron/keyring.json` (per the file-based scheme
// introduced for #366); a missing or empty file means no publishers
// are trusted, and the install pipeline fails closed on every call
// (per ADR-0004's failure-modes table). Users opt in to a publisher
// by adding its ed25519 public key to the keyring file.
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
	keyringPath := cstore.DefaultKeyringPath()
	keyring, err := cstore.LoadKeyring(keyringPath)
	if err != nil {
		log.Warn("failed to load keyring; falling back to empty (fail-closed)",
			"path", keyringPath, "error", err)
		keyring = cstore.NewEd25519Keyring()
	}
	return &cstore.Installer{
		Resolver: cstore.DefaultResolver(),
		Fetcher:  &cstore.HTTPFetcher{},
		Verifier: keyring,
		Store:    store,
	}
}
