// Package app wires together the Aileron control plane components and exposes
// them as an http.Handler. It is imported by the standalone server binary.
package app

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"github.com/ALRubinger/aileron/core/account"
	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/approval"
	"github.com/ALRubinger/aileron/core/draft"
	"github.com/ALRubinger/aileron/core/llm"
	"github.com/ALRubinger/aileron/core/source"
	calendarsource "github.com/ALRubinger/aileron/core/source/calendar"
	githubsource "github.com/ALRubinger/aileron/core/source/github"
	gmailsource "github.com/ALRubinger/aileron/core/source/gmail"
	slacksource "github.com/ALRubinger/aileron/core/source/slack"
	"github.com/ALRubinger/aileron/core/auth"
	githubauth "github.com/ALRubinger/aileron/core/auth/github"
	googleauth "github.com/ALRubinger/aileron/core/auth/google"
	"github.com/ALRubinger/aileron/core/config"
	"github.com/ALRubinger/aileron/core/connector"
	googlecalendar "github.com/ALRubinger/aileron/core/connector/calendar/google"
	"github.com/ALRubinger/aileron/core/connector/git/github"
	"github.com/ALRubinger/aileron/core/connector/payments/stripe"
	"github.com/ALRubinger/aileron/core/notify"
	"github.com/ALRubinger/aileron/core/policy"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/store/postgres"
	"github.com/ALRubinger/aileron/core/vault"
	"github.com/ALRubinger/aileron/enclave"
	"github.com/google/uuid"
)

// NewHandler creates a fully-wired Aileron control plane HTTP handler with
// in-memory stores, seeded policies, and registered connectors.
func NewHandler(log *slog.Logger) (http.Handler, error) {
	ctx := context.Background()

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

	// --- Connector registry ---
	registry := connector.NewRegistry()
	registry.Register(ctx, stripe.New())
	registry.Register(ctx, googlecalendar.New())
	registry.Register(ctx, github.New())

	// --- Vault ---
	// Start with in-memory vault; upgrade to Postgres when database is available.
	var v vault.Vault = vault.NewMemVault()
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
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
	notifier := notify.NewLogNotifier(log)

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
		log:            log,
		registry:       registry,
		policyEngine:   policyEngine,
		orchestrator:   orchestrator,
		vault:          v,
		notifier:       notifier,
		intents:        intentStore,
		approvals:      approvalStore,
		policies:       policyStore,
		grants:         grantStore,
		executions:     executionStore,
		connectors:     connectorStore,
		connectedAccounts: connectedAccountStore,
		drafts:            draftStore,
		instructions:      instructionStore,
		feedback:          feedbackStore,
		credentials:       credentialStore,
		fundingSources:    fundingSourceStore,
		traces:            traceStore,
		sourceRegistry:    sourceReg,
		enclaveClient:     enclaveClient,
		enclaveVerifier:   enclaveVerifier,
		teeCfg:            teeCfg,
		newID:             idGen,
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

	// Connected accounts — programmatic create (in addition to OAuth flow).
	mux.HandleFunc("POST /v1/connected-accounts", server.handleCreateConnectedAccount)

	// Draft lifecycle API.
	mux.HandleFunc("POST /v1/drafts", server.handleCreateDraft)
	mux.HandleFunc("GET /v1/drafts", server.handleListDrafts)
	mux.HandleFunc("GET /v1/drafts/", server.handleGetDraft)
	mux.HandleFunc("POST /v1/drafts/", server.handleDraftAction)

	registerDocsRoutes(mux)

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

		userKeyMaterialStore := mem.NewUserKeyMaterialStore()

		// Wire stores into apiServer for /me endpoints.
		server.enterprises = enterpriseStore
		server.users = userStore
		server.userAuthProviders = userAuthProviderStore
		server.userKeyMaterials = userKeyMaterialStore
		server.escrowTTL = authCfg.EscrowTTL()

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

		// LLM-powered draft generation pipeline.
		if authCfg.LLMEnabled() {
			researchClient := llm.NewAnthropicClient(authCfg.AnthropicAPIKey, authCfg.LLMModelResearch, llm.WithLogger(log))
			synthesisClient := llm.NewAnthropicClient(authCfg.AnthropicAPIKey, authCfg.LLMModelSynthesis, llm.WithLogger(log))
			prompts := draft.LoadPrompts()
			server.draftPipeline = draft.NewPipeline(researchClient, synthesisClient, sourceReg, pgConnectedAccountStore, pgInstructionStore, v, log, prompts)
			log.Info("enabled cloud draft generation",
				"research_model", authCfg.LLMModelResearch,
				"synthesis_model", authCfg.LLMModelSynthesis,
				"research_prompt_length", len(prompts.Research),
				"ghostwrite_prompt_length", len(prompts.Ghostwrite),
				"prompt_source", draft.PromptSource())
		}

		if authCfg.SlackEnabled() {
			server.slackSigningSecret = authCfg.SlackSigningSecret
			server.slackDedup = newSlackEventDedup()
			server.onSlackMessage = server.handleIncomingSlackMessage
			mux.HandleFunc("POST /v1/webhooks/slack/events", server.handleSlackEvent)
			mux.HandleFunc("POST /v1/webhooks/slack/interactions", server.handleSlackInteraction)
			log.Info("enabled Slack Events API webhook and interaction endpoints")
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
			UIRedirect:        authCfg.UIRedirectURL,
			RefreshTTL:        authCfg.RefreshTokenTTL,
			AutoVerifyEmail: authCfg.AutoVerifyEmail,
		})
		authHandler.RegisterRoutes(mux)

		skipPaths := map[string]bool{
			"/v1/health":                        true,
			"/v1/webhooks/slack/events":          true,
			"/v1/webhooks/slack/interactions":    true,
		}
		handler = auth.Middleware(tokenIssuer, skipPaths)(handler)
		log.Info("auth middleware enabled")
	}

	handler = loggingMiddleware(log, handler)
	handler = requestIDMiddleware(handler)
	handler = corsMiddleware(handler)

	return handler, nil
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
		sourceReg.Register(gmailsource.New())
		sourceReg.Register(calendarsource.New())
		log.Info("enabled Google connected accounts and source connectors (Gmail, Calendar)")
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
