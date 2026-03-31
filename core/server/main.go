// Package main is the entry point for the Aileron control plane server.
//
// The server wires together the core components — policy engine, approval
// orchestrator, connector registry, vault, notifiers, and audit store —
// and exposes the control plane API over HTTP.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/approval"
	"github.com/ALRubinger/aileron/core/connector"
	googlecalendar "github.com/ALRubinger/aileron/core/connector/calendar/google"
	"github.com/ALRubinger/aileron/core/connector/git/github"
	"github.com/ALRubinger/aileron/core/connector/payments/stripe"
	"github.com/ALRubinger/aileron/core/notify"
	"github.com/ALRubinger/aileron/core/policy"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/vault"
	"github.com/google/uuid"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(log); err != nil {
		log.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx := context.Background()
	cfg := configFromEnv()

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

	// --- Connector registry ---
	registry := connector.NewRegistry()
	registry.Register(ctx, stripe.New())
	registry.Register(ctx, googlecalendar.New())
	registry.Register(ctx, github.New())

	// --- Vault ---
	v := vault.NewMemVault()
	// Seed GitHub PAT from environment if available.
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

	// Seed default policies.
	if err := policy.SeedPolicies(ctx, policyStore); err != nil {
		return err
	}
	log.Info("seeded default policies")

	// --- Approval orchestrator ---
	idGen := func() string { return uuid.New().String() }
	orchestrator := approval.NewInMemoryOrchestrator(approvalStore, idGen)

	// --- Notifier ---
	notifier := notify.NewLogNotifier(log)

	// --- HTTP server ---
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
		credentials:    credentialStore,
		fundingSources: fundingSourceStore,
		traces:         traceStore,
		newID:          idGen,
	}
	api.HandlerFromMux(server, mux)

	// Register non-spec routes (docs, raw spec).
	registerDocsRoutes(mux)

	// Middleware chain: CORS → request ID → logging → routes.
	var handler http.Handler = mux
	handler = loggingMiddleware(log, handler)
	handler = requestIDMiddleware(handler)
	handler = corsMiddleware(handler)

	srv := &http.Server{
		Addr:         cfg.addr,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown on SIGINT / SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("server listening", "addr", cfg.addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("listen error", "error", err)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	return srv.Shutdown(shutdownCtx)
}

// apiServer implements the generated api.ServerInterface.
type apiServer struct {
	log            *slog.Logger
	registry       *connector.Registry
	policyEngine   *policy.RuleEngine
	orchestrator   *approval.InMemoryOrchestrator
	vault          *vault.MemVault
	notifier       notify.Notifier
	intents        *mem.IntentStore
	approvals      *mem.ApprovalStore
	policies       *mem.PolicyStore
	grants         *mem.GrantStore
	executions     *mem.ExecutionStore
	connectors     *mem.ConnectorStore
	credentials    *mem.CredentialStore
	fundingSources *mem.FundingSourceStore
	traces         *mem.TraceStore
	newID          func() string
}

// config holds runtime configuration sourced from environment variables.
type config struct {
	addr        string
	databaseURL string
}

func configFromEnv() config {
	addr := os.Getenv("AILERON_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	return config{
		addr:        addr,
		databaseURL: os.Getenv("DATABASE_URL"),
	}
}
