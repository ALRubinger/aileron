// Package main is the entry point for the Aileron control plane server.
//
// The server wires together the core components — policy engine, approval
// orchestrator, connector registry, vault, notifiers, and audit store —
// and exposes the control plane API over HTTP.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/connector"
	googlecalendar "github.com/ALRubinger/aileron/core/connector/calendar/google"
	"github.com/ALRubinger/aileron/core/connector/git/github"
	"github.com/ALRubinger/aileron/core/connector/payments/stripe"
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

	// --- Connector registry ---
	registry := connector.NewRegistry()
	registry.Register(ctx, stripe.New())
	registry.Register(ctx, googlecalendar.New())
	registry.Register(ctx, github.New())

	// --- HTTP server ---
	mux := http.NewServeMux()

	// Register generated API routes from the OpenAPI spec.
	server := &apiServer{log: log, registry: registry}
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
	log      *slog.Logger
	registry *connector.Registry
}

// GetHealth implements api.ServerInterface.
func (s *apiServer) GetHealth(w http.ResponseWriter, r *http.Request) {
	s.log.InfoContext(r.Context(), "health check")
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","service":"aileron","version":"0.1.0","timestamp":"%s"}`, time.Now().UTC().Format(time.RFC3339))
}

// --- Stub implementations (501 Not Implemented) ---

func notImplemented(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":    "not_implemented",
			"message": "This endpoint is not yet implemented",
		},
	})
}

func (s *apiServer) GetAnalyticsSummary(w http.ResponseWriter, r *http.Request, params api.GetAnalyticsSummaryParams) {
	notImplemented(w)
}

func (s *apiServer) ListApprovals(w http.ResponseWriter, r *http.Request, params api.ListApprovalsParams) {
	notImplemented(w)
}

func (s *apiServer) GetApproval(w http.ResponseWriter, r *http.Request, approvalId api.ApprovalId) {
	notImplemented(w)
}

func (s *apiServer) ApproveRequest(w http.ResponseWriter, r *http.Request, approvalId api.ApprovalId) {
	notImplemented(w)
}

func (s *apiServer) DenyRequest(w http.ResponseWriter, r *http.Request, approvalId api.ApprovalId) {
	notImplemented(w)
}

func (s *apiServer) ModifyRequest(w http.ResponseWriter, r *http.Request, approvalId api.ApprovalId) {
	notImplemented(w)
}

func (s *apiServer) ListConnectors(w http.ResponseWriter, r *http.Request, params api.ListConnectorsParams) {
	notImplemented(w)
}

func (s *apiServer) CreateConnector(w http.ResponseWriter, r *http.Request) {
	notImplemented(w)
}

func (s *apiServer) GetConnector(w http.ResponseWriter, r *http.Request, connectorId api.ConnectorId) {
	notImplemented(w)
}

func (s *apiServer) UpdateConnector(w http.ResponseWriter, r *http.Request, connectorId api.ConnectorId) {
	notImplemented(w)
}

func (s *apiServer) ListCredentials(w http.ResponseWriter, r *http.Request, params api.ListCredentialsParams) {
	notImplemented(w)
}

func (s *apiServer) CreateCredential(w http.ResponseWriter, r *http.Request) {
	notImplemented(w)
}

func (s *apiServer) GetExecutionGrant(w http.ResponseWriter, r *http.Request, grantId api.GrantId) {
	notImplemented(w)
}

func (s *apiServer) RunExecution(w http.ResponseWriter, r *http.Request) {
	notImplemented(w)
}

func (s *apiServer) GetExecution(w http.ResponseWriter, r *http.Request, executionId api.ExecutionId) {
	notImplemented(w)
}

func (s *apiServer) ExecutionCallback(w http.ResponseWriter, r *http.Request, executionId api.ExecutionId) {
	notImplemented(w)
}

func (s *apiServer) ListFundingSources(w http.ResponseWriter, r *http.Request, params api.ListFundingSourcesParams) {
	notImplemented(w)
}

func (s *apiServer) CreateFundingSource(w http.ResponseWriter, r *http.Request) {
	notImplemented(w)
}

func (s *apiServer) ListIntents(w http.ResponseWriter, r *http.Request, params api.ListIntentsParams) {
	notImplemented(w)
}

func (s *apiServer) CreateIntent(w http.ResponseWriter, r *http.Request) {
	notImplemented(w)
}

func (s *apiServer) GetIntent(w http.ResponseWriter, r *http.Request, intentId api.IntentId) {
	notImplemented(w)
}

func (s *apiServer) AppendIntentEvidence(w http.ResponseWriter, r *http.Request, intentId api.IntentId) {
	notImplemented(w)
}

func (s *apiServer) ListPolicies(w http.ResponseWriter, r *http.Request, params api.ListPoliciesParams) {
	notImplemented(w)
}

func (s *apiServer) CreatePolicy(w http.ResponseWriter, r *http.Request) {
	notImplemented(w)
}

func (s *apiServer) SimulatePolicy(w http.ResponseWriter, r *http.Request) {
	notImplemented(w)
}

func (s *apiServer) GetPolicy(w http.ResponseWriter, r *http.Request, policyId api.PolicyId) {
	notImplemented(w)
}

func (s *apiServer) UpdatePolicy(w http.ResponseWriter, r *http.Request, policyId api.PolicyId) {
	notImplemented(w)
}

func (s *apiServer) ListTraces(w http.ResponseWriter, r *http.Request, params api.ListTracesParams) {
	notImplemented(w)
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
