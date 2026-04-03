// Package app wires together the Aileron control plane components and exposes
// them as an http.Handler. It is imported by the standalone server binary and
// the MCP server's embedded mode.
package app

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/approval"
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
	mcpreg "github.com/ALRubinger/aileron/core/registry"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/store/postgres"
	"github.com/ALRubinger/aileron/core/vault"
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
	mcpServerStore := mem.NewMCPServerStore()
	credentialStore := mem.NewCredentialStore()
	fundingSourceStore := mem.NewFundingSourceStore()
	traceStore := mem.NewTraceStore()

	// --- Connector registry ---
	registry := connector.NewRegistry()
	registry.Register(ctx, stripe.New())
	registry.Register(ctx, googlecalendar.New())
	registry.Register(ctx, github.New())

	// --- Vault ---
	v := vault.NewMemVault()
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

	// --- MCP Registry client ---
	registryClient := mcpreg.NewClient(nil, log)
	if interval := os.Getenv("REGISTRY_REFRESH_INTERVAL"); interval != "" {
		if d, err := time.ParseDuration(interval); err == nil {
			registryClient = registryClient.WithRefreshInterval(d)
			log.Info("registry refresh interval overridden", "interval", d)
		} else {
			log.Warn("invalid REGISTRY_REFRESH_INTERVAL, using default", "value", interval, "error", err)
		}
	}
	registryClient.Start(ctx)

	// --- Notifier ---
	notifier := notify.NewLogNotifier(log)

	// --- HTTP handler ---
	mux := http.NewServeMux()

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
		mcpServers:     mcpServerStore,
		registryClient: registryClient,
		credentials:    credentialStore,
		fundingSources: fundingSourceStore,
		traces:         traceStore,
		newID:          idGen,
	}
	api.HandlerFromMux(server, mux)

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
		sessionStore := postgres.NewSessionStore(db)
		verificationCodeStore := postgres.NewVerificationCodeStore(db)

		// Wire stores into apiServer for /me endpoints.
		server.enterprises = enterpriseStore
		server.users = userStore

		tokenIssuer := auth.NewTokenIssuer(
			[]byte(authCfg.JWTSigningKey),
			authCfg.JWTIssuer,
			authCfg.AccessTokenTTL,
		)

		authRegistry := auth.NewRegistry()
		if authCfg.GoogleEnabled() {
			authRegistry.Register(googleauth.New(
				authCfg.GoogleClientID,
				authCfg.GoogleClientSecret,
			))
			log.Info("registered Google OAuth provider")
		}
		if authCfg.GitHubEnabled() {
			authRegistry.Register(githubauth.New(
				authCfg.GitHubClientID,
				authCfg.GitHubClientSecret,
			))
			log.Info("registered GitHub OAuth provider")
		}

		enforcer := auth.NewStoreEnforcer(enterpriseStore)

		mailer := auth.NewLogMailer(log)

		authHandler := auth.NewHandler(auth.HandlerConfig{
			Log:               log,
			Registry:          authRegistry,
			Enforcer:          enforcer,
			Issuer:            tokenIssuer,
			Users:             userStore,
			Enterprises:       enterpriseStore,
			Sessions:          sessionStore,
			VerificationCodes: verificationCodeStore,
			Mailer:            mailer,
			NewID:             idGen,
			UIRedirect:        authCfg.UIRedirectURL,
			RefreshTTL:        authCfg.RefreshTokenTTL,
			AutoVerifyEmail:   authCfg.AutoVerifyEmail,
			CallbackBaseURL:   authCfg.OAuthCallbackBaseURL,
			TrustedOrigins:    authCfg.TrustedOrigins,
		})
		authHandler.RegisterRoutes(mux)

		skipPaths := map[string]bool{
			"/v1/health": true,
		}
		handler = auth.Middleware(tokenIssuer, skipPaths)(handler)
		log.Info("auth middleware enabled")
	}

	handler = loggingMiddleware(log, handler)
	handler = requestIDMiddleware(handler)
	handler = corsMiddleware(handler)

	return handler, nil
}
