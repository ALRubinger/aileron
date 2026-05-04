package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/ALRubinger/aileron/internal/account"
	"github.com/ALRubinger/aileron/internal/action"
	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/sandbox"
	"github.com/ALRubinger/aileron/internal/approval"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/auth"
	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/comms"
	"github.com/ALRubinger/aileron/internal/config"
	connectorpkg "github.com/ALRubinger/aileron/internal/connector"
	"github.com/ALRubinger/aileron/internal/crypto"
	"github.com/ALRubinger/aileron/internal/draft"
	"github.com/ALRubinger/aileron/internal/enclave"
	"github.com/ALRubinger/aileron/internal/failure"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/notify"
	"github.com/ALRubinger/aileron/internal/policy"
	"github.com/ALRubinger/aileron/internal/source"
	"github.com/ALRubinger/aileron/internal/store"
	"github.com/ALRubinger/aileron/internal/store/mem"
	"github.com/ALRubinger/aileron/internal/store/postgres"
	"github.com/ALRubinger/aileron/internal/vault"
	"github.com/ALRubinger/aileron/internal/version"
)

// SlackAgentClient abstracts Slack's Agent/AI Apps API for testability.
type SlackAgentClient interface {
	SetStatus(ctx context.Context, botToken, channelID, threadTS, status string) error
	SetSuggestedPrompts(ctx context.Context, botToken, channelID, threadTS string, prompts []comms.SlackAgentPrompt) error
	SetTitle(ctx context.Context, botToken, channelID, threadTS, title string) error
	StartStream(ctx context.Context, botToken, channelID, threadTS string) (ts string, err error)
	AppendStream(ctx context.Context, botToken, channelID, ts, text string) error
	StopStream(ctx context.Context, botToken, channelID, ts string) error
	PostMessage(ctx context.Context, botToken, channelID, threadTS, text string) error
}

// apiServer implements the generated api.ServerInterface.
type apiServer struct {
	log                *slog.Logger
	registry           *connectorpkg.Registry
	policyEngine       *policy.RuleEngine
	orchestrator       *approval.InMemoryOrchestrator
	vault              vault.Vault
	systemVault        vault.Vault // infrastructure secrets (ADR-0020); nil when not configured
	notifier           notify.Notifier
	intents            *mem.IntentStore
	approvals          *mem.ApprovalStore
	policies           *mem.PolicyStore
	grants             *mem.GrantStore
	executions         *mem.ExecutionStore
	connectors         *mem.ConnectorStore
	credentials        *mem.CredentialStore
	fundingSources     *mem.FundingSourceStore
	traces             *mem.TraceStore
	connectedAccounts  store.ConnectedAccountStore
	accountService     *account.Registry           // nil when no account providers configured
	sourceRegistry     *source.Registry            // read-only source connectors for context retrieval
	draftPipeline      *draft.Pipeline             // nil when LLM not configured
	drafts             store.DraftStore            // draft lifecycle store
	instructions       store.UserInstructionStore  // user instructions for context store
	feedback           store.DraftFeedbackStore    // draft feedback signals for behavioral model
	slackSender        SlackSender                 // injectable for testing; defaults to comms.SendSlackMessage
	slackAgentClient   SlackAgentClient            // injectable for testing; defaults to defaultSlackAgentClient
	slackClientID      string                      // Slack app client ID (for bot install OAuth exchange)
	slackClientSecret  string                      // Slack app client secret (for bot install OAuth exchange)
	slackSigningSecret string                      // Slack Events API signing secret for webhook verification
	slackBotExchanger  slackBotTokenExchanger      // injectable for testing; defaults to defaultSlackBotTokenExchange
	slackTokenURL      string                      // overrides slackTokenURL const for testing
	slackDedup         *slackEventDedup            // deduplication cache for Slack events
	llmConfigs         store.LLMConfigStore        // per-user/per-org LLM provider config
	enterprises        store.EnterpriseStore       // nil when auth is disabled
	users              store.UserStore             // nil when auth is disabled
	userAuthProviders  store.UserAuthProviderStore // nil when auth is disabled
	userKeyMaterials   store.UserKeyMaterialStore  // nil when auth is disabled
	kekSessionCache    *auth.KEKSessionCache       // nil when auth is disabled
	enclaveClient      enclave.Client              // nil when TEE is disabled
	enclaveVerifier    enclave.Verifier            // nil when TEE is disabled
	teeCfg             *config.TEEConfig           // nil when TEE is disabled
	teeState           *teeState                   // nil when TEE is disabled
	escrowTTL          time.Duration               // TTL for auto-escrowed credentials
	grantCapabilityKey []byte                      // HMAC key for durable grant capabilities
	escrowIndex        sync.Map                    // vault path (string) -> escrow ID (string)
	escrowIndexStore   *postgres.EscrowIndexStore  // nil when auth is disabled; persists escrowIndex across restarts
	uiBaseURL          string                      // base URL for the web UI (for constructing unlock links)
	openAIProxy        http.Handler                // upstream proxy for /v1/chat/completions; nil disables the endpoint
	anthropicProxy     http.Handler                // upstream proxy for /v1/messages; nil disables the endpoint
	interceptEngine    interceptEngineHandle       // tool-call intercept engine; nil disables interception (proxy passthrough only)
	auditStore         *audit.MemStore             // ADR-0010 audit store; in-memory for v1, Postgres post-MVP
	auditRecorder      audit.Recorder              // ADR-0010 recorder; mints audit IDs and emits binding-lifecycle events
	vaultLocked        bool                        // ADR-0011 gate: when true, gateway endpoints refuse to serve until the vault is unlocked
	newID              func() string
	actions            *action.Store     // installed actions in ~/.aileron/actions/ (ADR-0003)
	executor           action.Executor   // synchronous action executor used by /v1/actions/{name}/run; nil falls back to stub
	installer          *cstore.Installer // connector install pipeline (ADR-0004); nil disables /v1/connectors/install
	sandboxRuntime     sandbox.Runtime   // WASM runtime for connector execution (ADR-0005); nil falls back to stub executor
	actionApprovals    *approval.ActionApprovalQueue // pending action-level approvals (manifest [approval] required = true); RunAction blocks on Decide
	actionApprovalTTL  time.Duration                 // how long RunAction holds the response open before timing out; default 5m, configurable for tests
	bindings           binding.Store     // capability bindings (ADR-0006); nil when no vault is wired
	oauth2Sessions     *oauth2Sessions   // ADR-0006 server-driven OAuth dance state; lazy-initialized on first use
	oauth2HTTPClient   *http.Client      // for OAuth token exchanges; nil → http.DefaultClient
}

// --- JSON helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		// Log to stderr since we don't have the logger here.
		// This should never happen with well-formed data — if it does,
		// it's a bug (e.g. a type with a broken MarshalJSON).
		slog.Error("writeJSON: marshal failed", "error", err)
		http.Error(w, `{"error":{"code":"internal_error","message":"response serialization failed"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(data)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, api.Error{
		Error: struct {
			Code      string                    `json:"code"`
			Details   *[]map[string]interface{} `json:"details,omitempty"`
			Message   string                    `json:"message"`
			RequestId *string                   `json:"request_id,omitempty"`
		}{
			Code:    code,
			Message: message,
		},
	})
}

func decodeBody(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

func isNotFound(err error) bool {
	var nf *store.ErrNotFound
	return errors.As(err, &nf)
}

// --- Health ---

func (s *apiServer) GetHealth(w http.ResponseWriter, r *http.Request) {
	// When TEE is configured, verify the enclave is reachable.
	if s.enclaveClient != nil {
		if err := s.enclaveClient.Ready(r.Context()); err != nil {
			s.log.Warn("health check: enclave not ready", "error", err)
			writeJSON(w, http.StatusServiceUnavailable, api.HealthResponse{
				Status:    "enclave_not_ready",
				Service:   "aileron",
				Version:   version.Version,
				Timestamp: time.Now().UTC(),
			})
			return
		}
	}

	writeJSON(w, http.StatusOK, api.HealthResponse{
		Status:    "ok",
		Service:   "aileron",
		Version:   version.Version,
		Timestamp: time.Now().UTC(),
	})
}

// --- Intents ---

func (s *apiServer) CreateIntent(w http.ResponseWriter, r *http.Request) {
	var req api.CreateIntentRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	ctx := r.Context()
	now := time.Now().UTC()
	intentID := "int_" + s.newID()
	traceID := "trc_" + s.newID()

	// Build the intent envelope.
	envelope := api.IntentEnvelope{
		IntentId:    intentID,
		WorkspaceId: req.WorkspaceId,
		Agent: api.ActorRef{
			Id:   req.AgentId,
			Type: api.Agent,
		},
		Action:    req.Action,
		Context:   req.Context,
		Status:    api.PendingPolicy,
		CreatedAt: now,
		UpdatedAt: now,
		Decision: api.Decision{
			Disposition: api.DecisionDispositionAllow,
			RiskLevel:   api.Low,
		},
	}

	if err := s.intents.Create(ctx, envelope); err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}

	// Emit intent.submitted audit event.
	s.emitTraceEvent(ctx, intentID, req.WorkspaceId, traceID, api.TraceEvent{
		EventId:   "evt_" + s.newID(),
		EventType: string(model.EventTypeIntentSubmitted),
		Actor:     api.ActorRef{Id: req.AgentId, Type: api.Agent},
		Timestamp: now,
	})

	// Evaluate policy.
	actionModel := apiActionToModel(req.Action)
	contextModel := apiContextToModel(req.Context)
	decision, err := s.policyEngine.Evaluate(ctx, policy.EvaluationRequest{
		WorkspaceID: req.WorkspaceId,
		AgentID:     req.AgentId,
		Action:      actionModel,
		Context:     contextModel,
	})
	if err != nil {
		s.log.ErrorContext(ctx, "policy evaluation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "policy_error", err.Error())
		return
	}

	// Emit policy.evaluated audit event.
	s.emitTraceEvent(ctx, intentID, req.WorkspaceId, traceID, api.TraceEvent{
		EventId:   "evt_" + s.newID(),
		EventType: string(model.EventTypePolicyEvaluated),
		Actor:     api.ActorRef{Id: "aileron", Type: api.Service},
		Timestamp: time.Now().UTC(),
		Payload: &map[string]interface{}{
			"disposition": string(decision.Disposition),
			"risk_level":  string(decision.RiskLevel),
		},
	})

	// Map model decision to API decision.
	apiDecision := modelDecisionToAPI(decision)

	switch decision.Disposition {
	case model.DispositionAllow:
		// Auto-approve: issue grant immediately.
		grantID := "grt_" + s.newID()
		grant, err := s.newExecutionGrant(ctx, envelope, grantID, now.Add(5*time.Minute), nil, userIDFromContext(ctx))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "grant_error", err.Error())
			return
		}
		if err := s.grants.Create(ctx, grant); err != nil {
			writeError(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		apiDecision.ExecutionGrantId = &grantID

		envelope.Status = api.Approved
		envelope.Decision = apiDecision
		envelope.UpdatedAt = time.Now().UTC()
		s.intents.Update(ctx, envelope)

		s.emitTraceEvent(ctx, intentID, req.WorkspaceId, traceID, api.TraceEvent{
			EventId:   "evt_" + s.newID(),
			EventType: string(model.EventTypeGrantIssued),
			Actor:     api.ActorRef{Id: "aileron", Type: api.Service},
			Timestamp: time.Now().UTC(),
			Payload:   &map[string]interface{}{"grant_id": grantID},
		})

	case model.DispositionRequireApproval:
		// Create approval request.
		apr, err := s.orchestrator.Request(ctx, approval.ApprovalRequest{
			IntentID:    intentID,
			WorkspaceID: req.WorkspaceId,
			Rationale:   fmt.Sprintf("Policy requires approval: %s", req.Action.Summary),
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "approval_error", err.Error())
			return
		}
		apiDecision.ApprovalId = &apr.ApprovalID
		ra := true
		apiDecision.RequiresApproval = &ra

		envelope.Status = api.PendingApproval
		envelope.Decision = apiDecision
		envelope.UpdatedAt = time.Now().UTC()
		s.intents.Update(ctx, envelope)

		s.emitTraceEvent(ctx, intentID, req.WorkspaceId, traceID, api.TraceEvent{
			EventId:   "evt_" + s.newID(),
			EventType: string(model.EventTypeApprovalRequested),
			Actor:     api.ActorRef{Id: "aileron", Type: api.Service},
			Timestamp: time.Now().UTC(),
			Payload:   &map[string]interface{}{"approval_id": apr.ApprovalID},
		})

		// Send notification.
		s.notifier.Notify(ctx, notify.Notification{
			ApprovalID:  apr.ApprovalID,
			IntentID:    intentID,
			WorkspaceID: req.WorkspaceId,
			Summary:     req.Action.Summary,
			ReviewURL:   fmt.Sprintf("/approvals/%s", apr.ApprovalID),
		})

	case model.DispositionDeny:
		envelope.Status = api.Denied
		envelope.Decision = apiDecision
		envelope.UpdatedAt = time.Now().UTC()
		s.intents.Update(ctx, envelope)

	default:
		envelope.Decision = apiDecision
		envelope.UpdatedAt = time.Now().UTC()
		s.intents.Update(ctx, envelope)
	}

	writeJSON(w, http.StatusCreated, envelope)
}

func (s *apiServer) GetIntent(w http.ResponseWriter, r *http.Request, intentId api.IntentId) {
	intent, err := s.intents.Get(r.Context(), intentId)
	if isNotFound(err) {
		writeError(w, http.StatusNotFound, "not_found", "intent not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, intent)
}

func (s *apiServer) ListIntents(w http.ResponseWriter, r *http.Request, params api.ListIntentsParams) {
	filter := store.IntentFilter{}
	if params.WorkspaceId != nil {
		filter.WorkspaceID = *params.WorkspaceId
	}
	if params.Status != nil {
		filter.Status = params.Status
	}
	if params.AgentId != nil {
		filter.AgentID = *params.AgentId
	}
	intents, err := s.intents.List(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.IntentListResponse{Items: &intents})
}

func (s *apiServer) AppendIntentEvidence(w http.ResponseWriter, r *http.Request, intentId api.IntentId) {
	var req api.AppendEvidenceRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx := r.Context()
	intent, err := s.intents.Get(ctx, intentId)
	if isNotFound(err) {
		writeError(w, http.StatusNotFound, "not_found", "intent not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	existing := []api.EvidenceItem{}
	if intent.Evidence != nil {
		existing = *intent.Evidence
	}
	existing = append(existing, req.Evidence...)
	intent.Evidence = &existing
	intent.UpdatedAt = time.Now().UTC()
	s.intents.Update(ctx, intent)
	writeJSON(w, http.StatusOK, intent)
}

// --- Approvals ---

func (s *apiServer) ListApprovals(w http.ResponseWriter, r *http.Request, params api.ListApprovalsParams) {
	filter := store.ApprovalFilter{
		WorkspaceID: params.WorkspaceId,
	}
	approvals, err := s.approvals.List(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.ApprovalListResponse{Items: &approvals})
}

func (s *apiServer) GetApproval(w http.ResponseWriter, r *http.Request, approvalId api.ApprovalId) {
	a, err := s.approvals.Get(r.Context(), approvalId)
	if isNotFound(err) {
		writeError(w, http.StatusNotFound, "not_found", "approval not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *apiServer) ApproveRequest(w http.ResponseWriter, r *http.Request, approvalId api.ApprovalId) {
	var req api.ApproveRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx := r.Context()

	apr, err := s.orchestrator.Approve(ctx, approvalId, approval.ApproveRequest{})
	if err != nil {
		writeError(w, http.StatusBadRequest, "approval_error", err.Error())
		return
	}

	// Issue execution grant.
	grantID := "grt_" + s.newID()

	// Update intent status.
	if intent, err := s.intents.Get(ctx, apr.IntentID); err == nil {
		grant, err := s.newExecutionGrant(ctx, intent, grantID, time.Now().UTC().Add(5*time.Minute), nil, userIDFromContext(ctx))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "grant_error", err.Error())
			return
		}
		if err := s.grants.Create(ctx, grant); err != nil {
			writeError(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		intent.Status = api.Approved
		intent.Decision.ExecutionGrantId = &grantID
		intent.UpdatedAt = time.Now().UTC()
		s.intents.Update(ctx, intent)
	} else {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}

	// Emit audit events.
	s.emitTraceEvent(ctx, apr.IntentID, apr.WorkspaceID, "", api.TraceEvent{
		EventId:   "evt_" + s.newID(),
		EventType: string(model.EventTypeApprovalApproved),
		Actor:     api.ActorRef{Id: "human", Type: api.Human},
		Timestamp: time.Now().UTC(),
		Payload:   &map[string]interface{}{"approval_id": approvalId},
	})
	s.emitTraceEvent(ctx, apr.IntentID, apr.WorkspaceID, "", api.TraceEvent{
		EventId:   "evt_" + s.newID(),
		EventType: string(model.EventTypeGrantIssued),
		Actor:     api.ActorRef{Id: "aileron", Type: api.Service},
		Timestamp: time.Now().UTC(),
		Payload:   &map[string]interface{}{"grant_id": grantID},
	})

	approvedStatus := api.ApprovalStatusApproved
	intentApproved := api.Approved
	writeJSON(w, http.StatusOK, api.ApprovalActionResponse{
		ApprovalId:       approvalId,
		Status:           approvedStatus,
		ExecutionGrantId: &grantID,
		IntentStatus:     &intentApproved,
	})
}

func (s *apiServer) DenyRequest(w http.ResponseWriter, r *http.Request, approvalId api.ApprovalId) {
	var req api.DenyRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx := r.Context()

	apr, err := s.orchestrator.Deny(ctx, approvalId, approval.DenyRequest{Reason: req.Reason})
	if err != nil {
		writeError(w, http.StatusBadRequest, "approval_error", err.Error())
		return
	}

	// Update intent status.
	if intent, err := s.intents.Get(ctx, apr.IntentID); err == nil {
		intent.Status = api.Denied
		intent.UpdatedAt = time.Now().UTC()
		s.intents.Update(ctx, intent)
	}

	s.emitTraceEvent(ctx, apr.IntentID, apr.WorkspaceID, "", api.TraceEvent{
		EventId:   "evt_" + s.newID(),
		EventType: string(model.EventTypeApprovalDenied),
		Actor:     api.ActorRef{Id: "human", Type: api.Human},
		Timestamp: time.Now().UTC(),
		Payload:   &map[string]interface{}{"approval_id": approvalId, "reason": req.Reason},
	})

	deniedStatus := api.ApprovalStatusDenied
	intentDenied := api.Denied
	writeJSON(w, http.StatusOK, api.ApprovalActionResponse{
		ApprovalId:   approvalId,
		Status:       deniedStatus,
		IntentStatus: &intentDenied,
	})
}

func (s *apiServer) ModifyRequest(w http.ResponseWriter, r *http.Request, approvalId api.ApprovalId) {
	var req api.ModifyApprovalRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx := r.Context()

	apr, err := s.orchestrator.Modify(ctx, approvalId, approval.ModifyRequest{
		Modifications: req.Modifications,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "approval_error", err.Error())
		return
	}

	// Issue execution grant with bounded parameters.
	grantID := "grt_" + s.newID()

	if intent, err := s.intents.Get(ctx, apr.IntentID); err == nil {
		grant, err := s.newExecutionGrant(ctx, intent, grantID, time.Now().UTC().Add(5*time.Minute), &req.Modifications, userIDFromContext(ctx))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "grant_error", err.Error())
			return
		}
		if err := s.grants.Create(ctx, grant); err != nil {
			writeError(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		intent.Status = api.Approved
		intent.Decision.ExecutionGrantId = &grantID
		intent.UpdatedAt = time.Now().UTC()
		s.intents.Update(ctx, intent)
	} else {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}

	modifiedStatus := api.ApprovalStatusModified
	intentApproved := api.Approved
	writeJSON(w, http.StatusOK, api.ApprovalActionResponse{
		ApprovalId:       approvalId,
		Status:           modifiedStatus,
		ExecutionGrantId: &grantID,
		IntentStatus:     &intentApproved,
	})
}

// --- Policies ---

func (s *apiServer) ListPolicies(w http.ResponseWriter, r *http.Request, params api.ListPoliciesParams) {
	filter := store.PolicyFilter{WorkspaceID: params.WorkspaceId}
	policies, err := s.policies.List(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.PolicyListResponse{Items: &policies})
}

func (s *apiServer) CreatePolicy(w http.ResponseWriter, r *http.Request) {
	var req api.CreatePolicyRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	now := time.Now().UTC()
	status := api.PolicyStatusActive
	if req.Status != nil {
		status = *req.Status
	}
	p := api.Policy{
		PolicyId:    "pol_" + s.newID(),
		WorkspaceId: req.WorkspaceId,
		Name:        req.Name,
		Description: req.Description,
		Environment: req.Environment,
		Rules:       req.Rules,
		Version:     1,
		Status:      status,
		CreatedAt:   &now,
		UpdatedAt:   &now,
	}
	if err := s.policies.Create(r.Context(), p); err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *apiServer) GetPolicy(w http.ResponseWriter, r *http.Request, policyId api.PolicyId) {
	p, err := s.policies.Get(r.Context(), policyId)
	if isNotFound(err) {
		writeError(w, http.StatusNotFound, "not_found", "policy not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *apiServer) UpdatePolicy(w http.ResponseWriter, r *http.Request, policyId api.PolicyId) {
	var req api.UpdatePolicyRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx := r.Context()
	p, err := s.policies.Get(ctx, policyId)
	if isNotFound(err) {
		writeError(w, http.StatusNotFound, "not_found", "policy not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if req.Name != nil {
		p.Name = *req.Name
	}
	if req.Description != nil {
		p.Description = req.Description
	}
	if req.Rules != nil {
		p.Rules = *req.Rules
	}
	if req.Status != nil {
		p.Status = *req.Status
	}
	p.Version++
	now := time.Now().UTC()
	p.UpdatedAt = &now
	s.policies.Update(ctx, p)
	writeJSON(w, http.StatusOK, p)
}

func (s *apiServer) SimulatePolicy(w http.ResponseWriter, r *http.Request) {
	var req api.PolicySimulationRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	actionModel := apiActionToModel(req.Action)
	contextModel := apiContextToModel(req.Context)
	decision, err := s.policyEngine.Evaluate(r.Context(), policy.EvaluationRequest{
		WorkspaceID: req.WorkspaceId,
		Action:      actionModel,
		Context:     contextModel,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "policy_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.PolicySimulationResponse{
		Decision: modelDecisionToAPI(decision),
	})
}

// --- Execution ---

func (s *apiServer) GetExecutionGrant(w http.ResponseWriter, r *http.Request, grantId api.GrantId) {
	g, err := s.grants.Get(r.Context(), grantId)
	if isNotFound(err) {
		writeError(w, http.StatusNotFound, "not_found", "grant not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (s *apiServer) RunExecution(w http.ResponseWriter, r *http.Request) {
	var req api.ExecutionRunRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx := r.Context()

	// Validate grant.
	grant, err := s.grants.Get(ctx, req.GrantId)
	if isNotFound(err) {
		writeError(w, http.StatusNotFound, "not_found", "grant not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if grant.Status != api.ExecutionGrantStatusActive {
		writeError(w, http.StatusBadRequest, "grant_inactive", "grant is not active")
		return
	}
	if time.Now().UTC().After(grant.ExpiresAt) {
		grant.Status = api.ExecutionGrantStatusExpired
		s.grants.Update(ctx, grant)
		writeError(w, http.StatusBadRequest, "grant_expired", "grant has expired")
		return
	}

	// Load intent for action details.
	intent, err := s.intents.Get(ctx, grant.IntentId)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}

	// Mark grant as consumed.
	grant.Status = api.ExecutionGrantStatusConsumed
	s.grants.Update(ctx, grant)

	// Create execution record.
	execID := "exe_" + s.newID()
	now := time.Now().UTC()
	exec := api.Execution{
		ExecutionId: execID,
		IntentId:    grant.IntentId,
		Status:      api.ExecutionStatusRunning,
		StartedAt:   now,
	}
	s.executions.Create(ctx, exec)

	// Update intent status.
	intent.Status = api.Executing
	intent.UpdatedAt = now
	s.intents.Update(ctx, intent)

	s.emitTraceEvent(ctx, grant.IntentId, intent.WorkspaceId, "", api.TraceEvent{
		EventId:   "evt_" + s.newID(),
		EventType: string(model.EventTypeExecutionStarted),
		Actor:     api.ActorRef{Id: "aileron", Type: api.Service},
		Timestamp: now,
		Payload:   &map[string]interface{}{"execution_id": execID},
	})

	// Determine connector and execute.
	connType, connProvider := resolveConnector(intent.Action.Type)
	conn, ok := s.registry.Get(ctx, connType, connProvider)
	if !ok {
		finishExecution(s, ctx, exec, intent, api.ExecutionStatusFailed, nil, "", "no connector for "+intent.Action.Type)
		writeError(w, http.StatusBadRequest, "no_connector", "no connector registered for action type: "+intent.Action.Type)
		return
	}

	// Build execution parameters from intent action domain.
	params := buildConnectorParams(intent.Action)
	if grant.BoundedParameters != nil {
		// Override with bounded params from approval modification.
		for k, v := range *grant.BoundedParameters {
			params[k] = v
		}
	}
	execUserID := userIDFromContext(ctx)

	// Resolve credential from vault. Try connected account first (user-scoped
	// OAuth token), fall back to infrastructure credential.
	vaultPath := s.resolveCredentialVaultPath(ctx, connProvider)
	secret, err := s.vault.Get(ctx, vaultPath)
	if err != nil {
		finishExecution(s, ctx, exec, intent, api.ExecutionStatusFailed, nil, "", "credential not found: "+vaultPath)
		writeError(w, http.StatusInternalServerError, "vault_error", "failed to resolve credential")
		return
	}
	issuedCapability, err := s.verifyExecutionGrantCapability(grant, intent, params, execUserID, vaultPath, connProvider, secret.Metadata.Type)
	if err != nil && s.enclaveClient != nil {
		finishExecution(s, ctx, exec, intent, api.ExecutionStatusFailed, nil, "", err.Error())
		writeError(w, http.StatusForbidden, "invalid_grant_capability", err.Error())
		return
	}

	// TEE mode: delegate execution to enclave. The credential stays
	// KEK-encrypted; only the enclave (which holds the user's KEK) can
	// decrypt it. The server never sees the plaintext credential.
	if s.enclaveClient != nil {
		if !s.requireEnclave(w, r) {
			finishExecution(s, ctx, exec, intent, api.ExecutionStatusFailed, nil, "", "enclave not ready")
			return
		}
		execReq := enclave.ExecuteRequest{
			RequestID:           execID,
			UserID:              execUserID,
			GrantID:             grant.GrantId,
			IntentID:            grant.IntentId,
			ActionType:          intent.Action.Type,
			ConnectorID:         connType + "/" + connProvider,
			VaultPath:           vaultPath,
			Provider:            connProvider,
			Parameters:          params,
			EncryptedCredential: secret.Value,
			CredentialType:      secret.Metadata.Type,
		}
		if issuedCapability != nil {
			applyIssuedCapabilityToExecuteRequest(&execReq, issuedCapability.Capability)
			execReq.Capability = issuedCapability
			execReq.IssuedCapability = issuedCapability
		}
		// If credential is escrowed in TEE memory, use the escrow ID
		// instead of sending the encrypted credential. This allows
		// execution without an active KEK session.
		if escrowID, ok := s.escrowIndex.Load(vaultPath); ok {
			execReq.EscrowID = escrowID.(string)
			execReq.EncryptedCredential = nil
		}
		enclaveResp, enclaveErr := s.enclaveClient.Execute(ctx, execReq)
		if enclaveErr != nil {
			finishExecution(s, ctx, exec, intent, api.ExecutionStatusFailed, nil, "", enclaveErr.Error())
			writeError(w, http.StatusInternalServerError, "enclave_error", enclaveErr.Error())
			return
		}

		enclaveStatus := api.ExecutionStatusSucceeded
		if enclaveResp.Status == "failed" {
			enclaveStatus = api.ExecutionStatusFailed
		}
		finishExecution(s, ctx, exec, intent, enclaveStatus, &enclaveResp.Output, enclaveResp.ReceiptRef, enclaveResp.Error)

		writeJSON(w, http.StatusAccepted, api.ExecutionRunResponse{
			ExecutionId: execID,
			Status:      api.Accepted,
		})
		return
	}

	// Direct mode: all credentials are encrypted — decrypt with user's KEK.
	kek := s.getUserKEK(execUserID)
	if kek == nil {
		finishExecution(s, ctx, exec, intent, api.ExecutionStatusFailed, nil, "", "vault locked — unlock to execute")
		writeError(w, 423, "vault_locked", "vault is locked — unlock with POST /v1/users/me/passphrase/unlock")
		return
	}
	credValue, decErr := crypto.Decrypt(secret.Value, kek)
	zeroBytes(kek)
	if decErr != nil {
		finishExecution(s, ctx, exec, intent, api.ExecutionStatusFailed, nil, "", "credential decryption failed")
		writeError(w, http.StatusInternalServerError, "decryption_error", "failed to decrypt credential")
		return
	}

	result, err := conn.Execute(ctx, connectorpkg.ExecutionRequest{
		GrantID:    grant.GrantId,
		IntentID:   grant.IntentId,
		ActionType: intent.Action.Type,
		Parameters: params,
		Credential: &connectorpkg.InjectedCredential{
			Type:  secret.Metadata.Type,
			Value: credValue,
		},
	})
	if err != nil {
		finishExecution(s, ctx, exec, intent, api.ExecutionStatusFailed, nil, "", err.Error())
		writeError(w, http.StatusInternalServerError, "execution_error", err.Error())
		return
	}

	status := api.ExecutionStatusSucceeded
	if result.Status == connectorpkg.ExecutionStatusFailed {
		status = api.ExecutionStatusFailed
	}
	finishExecution(s, ctx, exec, intent, status, &result.Output, result.ReceiptRef, result.Error)

	writeJSON(w, http.StatusAccepted, api.ExecutionRunResponse{
		ExecutionId: execID,
		Status:      api.Accepted,
		AcceptedAt:  &now,
	})
}

func (s *apiServer) GetExecution(w http.ResponseWriter, r *http.Request, executionId api.ExecutionId) {
	exec, err := s.executions.Get(r.Context(), executionId)
	if isNotFound(err) {
		writeError(w, http.StatusNotFound, "not_found", "execution not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, exec)
}

func (s *apiServer) ExecutionCallback(w http.ResponseWriter, r *http.Request, executionId api.ExecutionId) {
	var req api.ExecutionCallbackRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx := r.Context()
	exec, err := s.executions.Get(ctx, executionId)
	if isNotFound(err) {
		writeError(w, http.StatusNotFound, "not_found", "execution not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}

	now := time.Now().UTC()
	exec.Status = api.ExecutionStatus(req.Status)
	exec.FinishedAt = &now
	exec.Output = req.Output
	exec.ReceiptRef = req.ReceiptRef
	s.executions.Update(ctx, exec)
	writeJSON(w, http.StatusOK, exec)
}

// --- Connectors ---

func (s *apiServer) ListConnectors(w http.ResponseWriter, r *http.Request, params api.ListConnectorsParams) {
	filter := store.ConnectorFilter{WorkspaceID: params.WorkspaceId}
	connectors, err := s.connectors.List(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.ConnectorListResponse{Items: &connectors})
}

func (s *apiServer) CreateConnector(w http.ResponseWriter, r *http.Request) {
	var req api.CreateConnectorRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	conn := api.Connector{
		ConnectorId: "con_" + s.newID(),
		WorkspaceId: req.WorkspaceId,
		Name:        req.Name,
		Type:        req.Type,
		Provider:    req.Provider,
		Auth:        &req.Auth,
		Environment: req.Environment,
		Metadata:    req.Metadata,
		Status:      api.ConnectorStatusActive,
	}
	if err := s.connectors.Create(r.Context(), conn); err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, conn)
}

func (s *apiServer) GetConnector(w http.ResponseWriter, r *http.Request, connectorId api.ConnectorId) {
	c, err := s.connectors.Get(r.Context(), connectorId)
	if isNotFound(err) {
		writeError(w, http.StatusNotFound, "not_found", "connector not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *apiServer) UpdateConnector(w http.ResponseWriter, r *http.Request, connectorId api.ConnectorId) {
	var req api.UpdateConnectorRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx := r.Context()
	c, err := s.connectors.Get(ctx, connectorId)
	if isNotFound(err) {
		writeError(w, http.StatusNotFound, "not_found", "connector not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if req.Name != nil {
		c.Name = *req.Name
	}
	if req.Auth != nil {
		c.Auth = req.Auth
	}
	if req.Status != nil {
		c.Status = api.ConnectorStatus(*req.Status)
	}
	s.connectors.Update(ctx, c)
	writeJSON(w, http.StatusOK, c)
}

// --- Connector install pipeline (ADR-0004) ---

// InstallConnector runs the install pipeline for the supplied FQN+version.
// On success returns 201 with the InstalledConnector envelope (or 200 when
// the entry was already present in the store). On any pipeline failure
// returns a structured error per ADR-0010 — 422 for verification/hash
// failures so callers can distinguish "the request was well-formed but
// the artifact didn't pass" from a 4xx caused by malformed input.
func (s *apiServer) InstallConnector(w http.ResponseWriter, r *http.Request) {
	if s.installer == nil {
		writeError(w, http.StatusServiceUnavailable, "installer_disabled",
			"connector install pipeline is not configured")
		return
	}
	var req api.InstallConnectorRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	fqn, err := cstore.ParseFQN(req.Fqn)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_fqn", err.Error())
		return
	}
	if req.Version == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "version is required")
		return
	}
	ref, err := cstore.ParseRef(fqn.String() + "@" + req.Version)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	expected := ""
	if req.ExpectedHash != nil {
		expected = *req.ExpectedHash
	}
	res, err := s.installer.Install(r.Context(), cstore.InstallRequest{
		Ref:          ref,
		ExpectedHash: expected,
	})
	if err != nil {
		var aerr *cstore.Error
		if errors.As(err, &aerr) {
			writeError(w, installHTTPStatus(aerr.Class), string(aerr.Class), aerr.Message)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	out := api.InstalledConnector{
		Fqn:      ref.FQN.String(),
		Version:  ref.Version,
		Hash:     res.Hash,
		EntryDir: res.EntryDir,
	}
	if res.AlreadyInstalled {
		already := true
		out.AlreadyInstalled = &already
		writeJSON(w, http.StatusOK, out)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// installHTTPStatus maps a cstore failure class to an HTTP status. Per
// ADR-0010, every failure is structured; the choice of HTTP code reflects
// whether the failure is "your input is wrong" (4xx) or "the artifact
// didn't pass" (422 — request is well-formed but processing failed).
func installHTTPStatus(class cstore.FailureClass) int {
	switch class {
	case cstore.ClassUnknownScheme,
		cstore.ClassParseError,
		cstore.ClassValidationError:
		return http.StatusBadRequest
	case cstore.ClassFetchFailed,
		cstore.ClassMalformedTarball,
		cstore.ClassSignatureFailure,
		cstore.ClassHashMismatch,
		cstore.ClassFQNMismatch:
		return http.StatusUnprocessableEntity
	case cstore.ClassStoreUnwritable:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

// --- Actions (ADR-0003) ---

// ListActions returns the actions installed in ~/.aileron/actions/, plus a
// per-file load_errors slice for any files that failed to parse or validate
// (per ADR-0010). Aileron is user-level only at v1, so the action set is the
// running server's view of the local actions directory.
func (s *apiServer) ListActions(w http.ResponseWriter, r *http.Request) {
	if s.actions == nil {
		writeJSON(w, http.StatusOK, api.ActionListResponse{})
		return
	}
	res, err := s.actions.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "actions_load_failed", err.Error())
		return
	}
	loaded := s.actions.List()
	items := make([]api.Action, 0, len(loaded))
	for _, la := range loaded {
		items = append(items, manifestToAPI(la))
	}
	resp := api.ActionListResponse{Items: &items}
	if res.HasErrors() {
		errs := make([]api.ActionLoadError, 0, len(res.Errors))
		for _, e := range res.Errors {
			errs = append(errs, loadErrorToAPI(e))
		}
		resp.LoadErrors = &errs
	}
	writeJSON(w, http.StatusOK, resp)
}

// GetAction returns a single installed action by its bare local handle. The
// response carries the full parsed manifest plus the Markdown body, which
// doubles as the LLM-facing function description per ADR-0001/ADR-0003.
func (s *apiServer) GetAction(w http.ResponseWriter, r *http.Request, name string) {
	if s.actions == nil {
		writeError(w, http.StatusNotFound, "not_found", "action not found")
		return
	}
	la, err := s.actions.Get(name)
	if errors.Is(err, action.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "action not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "actions_lookup_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, manifestToAPI(la))
}

// RunAction synchronously executes an installed action with the supplied
// arguments and returns the result. Used by `cmd/aileron-mcp` to route
// MCP `tools/call` invocations into Aileron's action runtime when MCP
// is the canonical surface for action exposure (per the working-session
// decision on 2026-05-03).
//
// The vault must be unlocked (ADR-0011) before any action can resolve
// its bound credentials; calls against a locked vault return a 412
// FailureEnvelope with class `binding_required`.
//
// Action-side failures are surfaced as ADR-0010 envelopes with the
// canonical class-to-status mapping in failure.HTTPStatus. The 200
// response body matches [api.ActionRunResponse] and carries the
// successful Result.Content payload alongside the audit ID.
func (s *apiServer) RunAction(w http.ResponseWriter, r *http.Request, name string) {
	if s.vaultLocked {
		writeVaultLocked(w)
		return
	}
	if s.actions == nil || s.executor == nil {
		writeError(w, http.StatusNotFound, "not_found", "action not found")
		return
	}
	loaded, err := s.actions.Get(name)
	if errors.Is(err, action.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "action not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "actions_lookup_failed", err.Error())
		return
	}

	var req api.ActionRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid JSON request body")
		return
	}
	args := map[string]any{}
	if req.Args != nil {
		args = *req.Args
	}

	// Approval gate. When the action's manifest declares
	// `[approval] required = true` the runtime registers a pending
	// approval and holds the response open until the user decides
	// (via the webapp at #418 or `aileron approval` CLI). The agent
	// learns to surface the approval URL to the user during this
	// pause via templated MCP tool descriptions in `aileron-mcp`.
	if loaded.Manifest.ApprovalRequired() && s.actionApprovals != nil {
		connFQN := ""
		if len(loaded.Manifest.Execute) > 0 {
			connFQN = loaded.Manifest.Execute[0].Connector
		}
		sessionID := r.Header.Get("X-Aileron-Session-Id")
		entry := s.actionApprovals.Register(name, connFQN, sessionID, args)
		decision, waitErr := entry.Wait(r.Context(), s.actionApprovalTimeout())
		if errors.Is(waitErr, approval.ErrActionApprovalTimeout) {
			writeError(w, http.StatusRequestTimeout, "approval_timeout",
				"action requires user approval; no decision received before timeout")
			return
		}
		if waitErr != nil {
			// Most likely r.Context() cancelled (client disconnect);
			// surface as a generic failure for the audit log. Don't
			// emit a body when the client is gone.
			writeError(w, http.StatusInternalServerError, "approval_wait_error", waitErr.Error())
			return
		}
		if !decision.Approved {
			msg := "user denied approval"
			if decision.Reason != "" {
				msg = msg + ": " + decision.Reason
			}
			writeError(w, http.StatusForbidden, "approval_denied", msg)
			return
		}
		// Approved; fall through to execute.
	}

	result, err := s.executor.Execute(r.Context(), name, args)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "executor_error", err.Error())
		return
	}
	if result.Failure != nil {
		failure.WriteHTTP(w, result.Failure)
		return
	}

	// Mint an audit ID so callers have a stable back-reference into the
	// audit log per ADR-0010. The recorder may also produce one
	// upstream; both forms are acceptable.
	auditID := ""
	if s.newID != nil {
		auditID = s.newID()
	}
	resp := api.ActionRunResponse{AuditId: auditID}
	if result.Content != "" {
		content := result.Content
		resp.Result = &content
	}
	writeJSON(w, http.StatusOK, resp)
}

func manifestToAPI(la action.LoadedAction) api.Action {
	m := la.Manifest
	connectors := make([]api.ActionRequiresConnector, 0, len(m.Requires.Connectors))
	for _, c := range m.Requires.Connectors {
		connectors = append(connectors, api.ActionRequiresConnector{
			Name:         c.Name,
			Version:      c.Version,
			Hash:         c.Hash,
			Capabilities: append([]string(nil), c.Capabilities...),
		})
	}
	steps := make([]api.ActionExecuteStep, 0, len(m.Execute))
	for _, st := range m.Execute {
		var inputs *map[string]interface{}
		if st.Inputs != nil {
			cp := make(map[string]interface{}, len(st.Inputs))
			for k, v := range st.Inputs {
				cp[k] = v
			}
			inputs = &cp
		}
		steps = append(steps, api.ActionExecuteStep{
			Id:         st.ID,
			Connector:  st.Connector,
			Op:         st.Op,
			Idempotent: st.Idempotent,
			Inputs:     inputs,
		})
	}
	body := m.Body
	path := la.Path
	out := api.Action{
		Name:    m.Name,
		Version: m.Version,
		Source:  m.Source,
		Requires: api.ActionRequires{
			Connectors: &connectors,
		},
		Match:   api.ActionMatch{Intent: m.Match.Intent},
		Execute: steps,
	}
	if body != "" {
		out.Body = &body
	}
	if path != "" {
		out.Path = &path
	}
	if m.Approval != nil {
		req := m.Approval.Required
		out.Approval = &api.ActionApprovalPolicy{Required: &req}
	}
	if len(m.Inputs) > 0 {
		inputs := make([]api.ActionInput, 0, len(m.Inputs))
		for _, in := range m.Inputs {
			ai := api.ActionInput{
				Name:        in.Name,
				Type:        api.ActionInputType(in.Type),
				Description: in.Description,
			}
			if in.Required != nil {
				r := *in.Required
				ai.Required = &r
			}
			inputs = append(inputs, ai)
		}
		out.Inputs = &inputs
	}
	return out
}

func loadErrorToAPI(e *action.Error) api.ActionLoadError {
	out := api.ActionLoadError{
		Class:   string(e.Class),
		Message: e.Message,
		File:    e.File,
	}
	if e.Line > 0 {
		line := e.Line
		out.Line = &line
	}
	if e.Boundary != "" {
		b := string(e.Boundary)
		out.Boundary = &b
	}
	return out
}

// --- Credentials ---

func (s *apiServer) ListCredentials(w http.ResponseWriter, r *http.Request, params api.ListCredentialsParams) {
	filter := store.CredentialFilter{WorkspaceID: params.WorkspaceId}
	creds, err := s.credentials.List(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.CredentialListResponse{Items: &creds})
}

func (s *apiServer) CreateCredential(w http.ResponseWriter, r *http.Request) {
	var req api.CreateCredentialRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	cred := api.CredentialReference{
		CredentialId: "crd_" + s.newID(),
		WorkspaceId:  req.WorkspaceId,
		Name:         req.Name,
		Type:         api.CredentialReferenceType(req.Type),
		VaultPath:    req.VaultPath,
		Environment:  req.Environment,
		Metadata:     req.Metadata,
	}
	if err := s.credentials.Create(r.Context(), cred); err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, cred)
}

// --- Funding Sources ---

func (s *apiServer) ListFundingSources(w http.ResponseWriter, r *http.Request, params api.ListFundingSourcesParams) {
	filter := store.FundingSourceFilter{WorkspaceID: params.WorkspaceId}
	sources, err := s.fundingSources.List(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.FundingSourceListResponse{Items: &sources})
}

func (s *apiServer) CreateFundingSource(w http.ResponseWriter, r *http.Request) {
	var req api.CreateFundingSourceRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	fs := api.FundingSource{
		FundingSourceId: "fnd_" + s.newID(),
		WorkspaceId:     req.WorkspaceId,
		Name:            req.Name,
		Type:            api.FundingSourceType(req.Type),
		Currency:        req.Currency,
		Metadata:        req.Metadata,
		Status:          api.FundingSourceStatusActive,
	}
	if err := s.fundingSources.Create(r.Context(), fs); err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, fs)
}

// --- Traces ---

func (s *apiServer) ListTraces(w http.ResponseWriter, r *http.Request, params api.ListTracesParams) {
	filter := mem.TraceFilter{WorkspaceID: params.WorkspaceId}
	traces, err := s.traces.List(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.TraceListResponse{Items: &traces})
}

// --- Analytics (stub) ---

func (s *apiServer) GetAnalyticsSummary(w http.ResponseWriter, r *http.Request, params api.GetAnalyticsSummaryParams) {
	writeJSON(w, http.StatusOK, api.AnalyticsSummary{})
}

// --- Helpers ---

func (s *apiServer) emitTraceEvent(ctx context.Context, intentID, workspaceID, traceID string, event api.TraceEvent) {
	if traceID == "" {
		traceID = "trc_" + s.newID()
	}
	s.traces.Append(ctx, intentID, workspaceID, traceID, event)
}

// executeGrantResult is the outcome of an internal grant execution.
type executeGrantResult struct {
	ExecutionID string
	Status      api.ExecutionStatus
	Output      map[string]any
	ReceiptRef  string
	Error       string
}

// executeGrant runs an execution grant internally — used by both the HTTP
// RunExecution handler and the Slack approve_intent flow. It validates the
// grant, resolves the connector and credentials, and executes.
func (s *apiServer) executeGrant(ctx context.Context, grantID, userID string) (*executeGrantResult, error) {
	grant, err := s.grants.Get(ctx, grantID)
	if err != nil {
		return nil, fmt.Errorf("grant not found: %w", err)
	}
	if grant.Status != api.ExecutionGrantStatusActive {
		return nil, fmt.Errorf("grant is not active (status: %s)", grant.Status)
	}
	if time.Now().UTC().After(grant.ExpiresAt) {
		grant.Status = api.ExecutionGrantStatusExpired
		s.grants.Update(ctx, grant)
		return nil, fmt.Errorf("grant has expired")
	}

	intent, err := s.intents.Get(ctx, grant.IntentId)
	if err != nil {
		return nil, fmt.Errorf("intent not found: %w", err)
	}

	// Mark grant as consumed.
	grant.Status = api.ExecutionGrantStatusConsumed
	s.grants.Update(ctx, grant)

	// Create execution record.
	execID := "exe_" + s.newID()
	now := time.Now().UTC()
	exec := api.Execution{
		ExecutionId: execID,
		IntentId:    grant.IntentId,
		Status:      api.ExecutionStatusRunning,
		StartedAt:   now,
	}
	s.executions.Create(ctx, exec)

	intent.Status = api.Executing
	intent.UpdatedAt = now
	s.intents.Update(ctx, intent)

	// Determine connector.
	connType, connProvider := resolveConnector(intent.Action.Type)
	conn, ok := s.registry.Get(ctx, connType, connProvider)
	if !ok {
		finishExecution(s, ctx, exec, intent, api.ExecutionStatusFailed, nil, "", "no connector for "+intent.Action.Type)
		return &executeGrantResult{ExecutionID: execID, Status: api.ExecutionStatusFailed, Error: "no connector"}, nil
	}

	// Build params. If the intent was created from the Slack flow, the
	// domain fields are in metadata (model types, not API types). Extract
	// them directly.
	params := buildConnectorParams(intent.Action)
	if intent.Action.Metadata != nil {
		if domainRaw, ok := (*intent.Action.Metadata)["_model_domain"]; ok {
			if domainJSON, err := json.Marshal(domainRaw); err == nil {
				var domain model.DomainAction
				if json.Unmarshal(domainJSON, &domain) == nil {
					params = buildModelDomainParams(intent.Action.Type, domain)
				}
			}
		}
	}
	if grant.BoundedParameters != nil {
		for k, v := range *grant.BoundedParameters {
			params[k] = v
		}
	}

	// Resolve credential — inject userID into context for connected account lookup.
	credCtx := ctx
	if userID != "" {
		credCtx = contextWithUser(ctx, userID)
	}
	vaultPath := s.resolveCredentialVaultPath(credCtx, connProvider)
	secret, err := s.vault.Get(ctx, vaultPath)
	if err != nil {
		finishExecution(s, ctx, exec, intent, api.ExecutionStatusFailed, nil, "", "credential not found: "+vaultPath)
		return &executeGrantResult{ExecutionID: execID, Status: api.ExecutionStatusFailed, Error: "credential not found"}, nil
	}
	issuedCapability, err := s.verifyExecutionGrantCapability(grant, intent, params, userID, vaultPath, connProvider, secret.Metadata.Type)
	if err != nil && s.enclaveClient != nil {
		finishExecution(s, ctx, exec, intent, api.ExecutionStatusFailed, nil, "", err.Error())
		return &executeGrantResult{ExecutionID: execID, Status: api.ExecutionStatusFailed, Error: err.Error()}, nil
	}

	// TEE mode.
	if s.enclaveClient != nil {
		if err := s.enclaveClient.Ready(ctx); err != nil {
			finishExecution(s, ctx, exec, intent, api.ExecutionStatusFailed, nil, "", "enclave not ready")
			return &executeGrantResult{ExecutionID: execID, Status: api.ExecutionStatusFailed, Error: "enclave not ready"}, nil
		}
		execReq := enclave.ExecuteRequest{
			RequestID:           execID,
			UserID:              userID,
			GrantID:             grant.GrantId,
			IntentID:            grant.IntentId,
			ActionType:          intent.Action.Type,
			ConnectorID:         connType + "/" + connProvider,
			VaultPath:           vaultPath,
			Provider:            connProvider,
			Parameters:          params,
			EncryptedCredential: secret.Value,
			CredentialType:      secret.Metadata.Type,
		}
		if issuedCapability != nil {
			applyIssuedCapabilityToExecuteRequest(&execReq, issuedCapability.Capability)
			execReq.Capability = issuedCapability
			execReq.IssuedCapability = issuedCapability
		}
		if escrowID, ok := s.escrowIndex.Load(vaultPath); ok {
			execReq.EscrowID = escrowID.(string)
			execReq.EncryptedCredential = nil
		}
		enclaveResp, enclaveErr := s.enclaveClient.Execute(ctx, execReq)
		if enclaveErr != nil {
			finishExecution(s, ctx, exec, intent, api.ExecutionStatusFailed, nil, "", enclaveErr.Error())
			return &executeGrantResult{ExecutionID: execID, Status: api.ExecutionStatusFailed, Error: enclaveErr.Error()}, nil
		}
		enclaveStatus := api.ExecutionStatusSucceeded
		if enclaveResp.Status == "failed" {
			enclaveStatus = api.ExecutionStatusFailed
		}
		finishExecution(s, ctx, exec, intent, enclaveStatus, &enclaveResp.Output, enclaveResp.ReceiptRef, enclaveResp.Error)
		return &executeGrantResult{
			ExecutionID: execID,
			Status:      enclaveStatus,
			Output:      enclaveResp.Output,
			ReceiptRef:  enclaveResp.ReceiptRef,
			Error:       enclaveResp.Error,
		}, nil
	}

	// Direct mode.
	kek := s.getUserKEK(userID)
	if kek == nil {
		finishExecution(s, ctx, exec, intent, api.ExecutionStatusFailed, nil, "", "vault locked")
		return &executeGrantResult{ExecutionID: execID, Status: api.ExecutionStatusFailed, Error: "vault locked"}, nil
	}
	credValue, decErr := crypto.Decrypt(secret.Value, kek)
	zeroBytes(kek)
	if decErr != nil {
		finishExecution(s, ctx, exec, intent, api.ExecutionStatusFailed, nil, "", "credential decryption failed")
		return &executeGrantResult{ExecutionID: execID, Status: api.ExecutionStatusFailed, Error: "credential decryption failed"}, nil
	}

	result, err := conn.Execute(ctx, connectorpkg.ExecutionRequest{
		GrantID:    grant.GrantId,
		IntentID:   grant.IntentId,
		ActionType: intent.Action.Type,
		Parameters: params,
		Credential: &connectorpkg.InjectedCredential{
			Type:  secret.Metadata.Type,
			Value: credValue,
		},
	})
	if err != nil {
		finishExecution(s, ctx, exec, intent, api.ExecutionStatusFailed, nil, "", err.Error())
		return &executeGrantResult{ExecutionID: execID, Status: api.ExecutionStatusFailed, Error: err.Error()}, nil
	}

	status := api.ExecutionStatusSucceeded
	if result.Status == connectorpkg.ExecutionStatusFailed {
		status = api.ExecutionStatusFailed
	}
	finishExecution(s, ctx, exec, intent, status, &result.Output, result.ReceiptRef, result.Error)
	return &executeGrantResult{
		ExecutionID: execID,
		Status:      status,
		Output:      result.Output,
		ReceiptRef:  result.ReceiptRef,
		Error:       result.Error,
	}, nil
}

func finishExecution(s *apiServer, ctx context.Context, exec api.Execution, intent api.IntentEnvelope, status api.ExecutionStatus, output *map[string]interface{}, receiptRef, errMsg string) {
	now := time.Now().UTC()
	exec.Status = status
	exec.FinishedAt = &now
	exec.Output = output
	if receiptRef != "" {
		exec.ReceiptRef = &receiptRef
	}
	s.executions.Update(ctx, exec)

	if status == api.ExecutionStatusSucceeded {
		intent.Status = api.Succeeded
	} else {
		intent.Status = api.Failed
	}
	intent.UpdatedAt = now
	s.intents.Update(ctx, intent)

	eventType := model.EventTypeExecutionSucceeded
	if status != api.ExecutionStatusSucceeded {
		eventType = model.EventTypeExecutionFailed
	}
	s.emitTraceEvent(ctx, intent.IntentId, intent.WorkspaceId, "", api.TraceEvent{
		EventId:   "evt_" + s.newID(),
		EventType: string(eventType),
		Actor:     api.ActorRef{Id: "aileron", Type: api.Service},
		Timestamp: now,
		Payload:   &map[string]interface{}{"execution_id": exec.ExecutionId, "status": string(status)},
	})
}

// resolveCredentialVaultPath returns the vault path for execution credentials.
// It first checks whether the authenticated user has a connected account for
// the provider (user-scoped OAuth token), falling back to the infrastructure
// credential path if none is found.
func (s *apiServer) resolveCredentialVaultPath(ctx context.Context, connProvider string) string {
	// Map connector provider to connected account provider.
	var acctProvider model.ConnectedAccountProvider
	switch connProvider {
	case "gmail":
		acctProvider = model.ConnectedAccountProviderGmail
	case "google_calendar":
		acctProvider = model.ConnectedAccountProviderGoogleCalendar
	case "github":
		acctProvider = model.ConnectedAccountProviderGitHub
	}

	if acctProvider != "" {
		var userID string
		if claims := auth.ClaimsFromContext(ctx); claims != nil {
			userID = claims.Subject
		}
		if userID != "" && s.connectedAccounts != nil {
			accounts, err := s.connectedAccounts.List(ctx, store.ConnectedAccountFilter{
				UserID: userID,
			})
			if err == nil {
				for _, acct := range accounts {
					if acct.Provider == acctProvider && acct.Status == model.ConnectedAccountStatusActive {
						return acct.VaultPath()
					}
				}
			}
		}
	}

	// Fall back to infrastructure credential.
	return fmt.Sprintf("connectors/%s/default", connProvider)
}

// enclaveReady checks whether the enclave is reachable. Returns nil if TEE
// is not configured or the enclave is healthy. Returns an error if TEE is
// configured but the enclave is unreachable.
func (s *apiServer) enclaveReady(ctx context.Context) error {
	if s.enclaveClient == nil {
		return nil
	}
	return s.enclaveClient.Ready(ctx)
}

// requireEnclave checks enclave readiness and writes a 503 error if it's
// unavailable. Returns true if the enclave is ready (or not configured).
func (s *apiServer) requireEnclave(w http.ResponseWriter, r *http.Request) bool {
	if err := s.enclaveReady(r.Context()); err != nil {
		s.log.Error("enclave not ready", "error", err)
		writeError(w, http.StatusServiceUnavailable, "enclave_not_ready",
			"Aileron is still starting up — please try again in a moment.")
		return false
	}
	return true
}

// buildModelDomainParams extracts connector parameters from a model.DomainAction.
// Used when the intent was created internally (Slack flow) with model types
// rather than API types.
func buildModelDomainParams(actionType string, domain model.DomainAction) map[string]any {
	params := map[string]any{"action_type": actionType}

	if domain.Email != nil {
		e := domain.Email
		params["to"] = e.To
		params["cc"] = e.CC
		params["bcc"] = e.BCC
		params["subject"] = e.Subject
		params["body_text"] = e.BodyText
		params["body_html"] = e.BodyHTML
		params["send_mode"] = e.SendMode
		params["thread_ref"] = e.ThreadRef
	}
	if domain.Calendar != nil {
		c := domain.Calendar
		params["title"] = c.Title
		params["description"] = c.Description
		params["timezone"] = c.Timezone
		params["location"] = c.Location
		params["calendar_id"] = c.CalendarID
		params["conference_type"] = c.ConferenceType
		params["visibility"] = c.Visibility
		if c.StartTime != nil {
			params["start_time"] = c.StartTime.Format("2006-01-02T15:04:05")
		}
		if c.EndTime != nil {
			params["end_time"] = c.EndTime.Format("2006-01-02T15:04:05")
		}
		if len(c.Attendees) > 0 {
			var attendees []map[string]any
			for _, a := range c.Attendees {
				attendees = append(attendees, map[string]any{
					"email": a.Email, "name": a.Name, "optional": a.Optional,
				})
			}
			params["attendees"] = attendees
		}
	}
	if domain.Git != nil {
		g := domain.Git
		params["repository"] = g.Repository
		params["branch"] = g.Branch
		params["base_branch"] = g.BaseBranch
		params["pr_title"] = g.PRTitle
		params["pr_body"] = g.PRBody
		params["issue_title"] = g.IssueTitle
		params["issue_body"] = g.IssueBody
		if len(g.IssueLabels) > 0 {
			params["issue_labels"] = g.IssueLabels
		}
		if len(g.IssueAssignees) > 0 {
			params["issue_assignees"] = g.IssueAssignees
		}
	}

	return params
}

func resolveConnector(actionType string) (connType, provider string) {
	switch {
	case len(actionType) >= 5 && actionType[:5] == "email":
		return "email", "gmail"
	case len(actionType) >= 3 && actionType[:3] == "git":
		return "git", "github"
	case len(actionType) >= 7 && actionType[:7] == "payment":
		return "payments", "stripe"
	case len(actionType) >= 8 && actionType[:8] == "calendar":
		return "calendar", "google_calendar"
	default:
		return "", ""
	}
}

func buildConnectorParams(action api.ActionIntent) map[string]any {
	params := map[string]any{
		"action_type": action.Type,
		"summary":     action.Summary,
	}
	if action.Domain != nil {
		if action.Domain.Git != nil {
			g := action.Domain.Git
			if g.Repository != nil {
				params["repository"] = *g.Repository
			}
			if g.Branch != nil {
				params["branch"] = *g.Branch
			}
			if g.BaseBranch != nil {
				params["base_branch"] = *g.BaseBranch
			}
			if g.PrTitle != nil {
				params["pr_title"] = *g.PrTitle
			}
			if g.PrBody != nil {
				params["pr_body"] = *g.PrBody
			}
			if g.Labels != nil {
				params["labels"] = *g.Labels
			}
			if g.Reviewers != nil {
				params["reviewers"] = *g.Reviewers
			}
			if g.IssueTitle != nil {
				params["issue_title"] = *g.IssueTitle
			}
			if g.IssueBody != nil {
				params["issue_body"] = *g.IssueBody
			}
			if g.IssueLabels != nil {
				params["issue_labels"] = *g.IssueLabels
			}
			if g.IssueAssignees != nil {
				params["issue_assignees"] = *g.IssueAssignees
			}
		}
		if action.Domain.Payment != nil {
			p := action.Domain.Payment
			if p.Amount != nil {
				params["amount"] = p.Amount.Amount
				params["currency"] = p.Amount.Currency
			}
			if p.VendorName != nil {
				params["vendor_name"] = *p.VendorName
			}
		}
		if action.Domain.Email != nil {
			e := action.Domain.Email
			if e.To != nil {
				params["to"] = *e.To
			}
			if e.Cc != nil {
				params["cc"] = *e.Cc
			}
			if e.Bcc != nil {
				params["bcc"] = *e.Bcc
			}
			if e.Subject != nil {
				params["subject"] = *e.Subject
			}
			if e.BodyText != nil {
				params["body_text"] = *e.BodyText
			}
			if e.BodyHtml != nil {
				params["body_html"] = *e.BodyHtml
			}
			if e.SendMode != nil {
				params["send_mode"] = string(*e.SendMode)
			}
			if e.ThreadRef != nil {
				params["thread_ref"] = *e.ThreadRef
			}
		}
		if action.Domain.Calendar != nil {
			c := action.Domain.Calendar
			if c.Title != nil {
				params["title"] = *c.Title
			}
			if c.Description != nil {
				params["description"] = *c.Description
			}
			if c.StartTime != nil {
				params["start_time"] = *c.StartTime
			}
			if c.EndTime != nil {
				params["end_time"] = *c.EndTime
			}
			if c.Timezone != nil {
				params["timezone"] = *c.Timezone
			}
			if c.Location != nil {
				params["location"] = *c.Location
			}
			if c.CalendarId != nil {
				params["calendar_id"] = *c.CalendarId
			}
			if c.ConferenceType != nil {
				params["conference_type"] = string(*c.ConferenceType)
			}
			if c.Attendees != nil {
				params["attendees"] = *c.Attendees
			}
			if c.Visibility != nil {
				params["visibility"] = string(*c.Visibility)
			}
		}
	}
	return params
}

// --- Model conversion ---

func apiActionToModel(action api.ActionIntent) model.ActionIntent {
	m := model.ActionIntent{
		Type:    action.Type,
		Summary: action.Summary,
	}
	if action.Justification != nil {
		m.Justification = *action.Justification
	}
	if action.Target != nil {
		m.Target = model.ActionTarget{
			Kind: string(action.Target.Kind),
		}
		if action.Target.Id != nil {
			m.Target.ID = *action.Target.Id
		}
		if action.Target.DisplayName != nil {
			m.Target.DisplayName = *action.Target.DisplayName
		}
	}
	if action.Domain != nil {
		if action.Domain.Git != nil {
			g := action.Domain.Git
			mg := &model.GitAction{}
			if g.Provider != nil {
				mg.Provider = string(*g.Provider)
			}
			if g.Repository != nil {
				mg.Repository = *g.Repository
			}
			if g.Branch != nil {
				mg.Branch = *g.Branch
			}
			if g.BaseBranch != nil {
				mg.BaseBranch = *g.BaseBranch
			}
			if g.PrTitle != nil {
				mg.PRTitle = *g.PrTitle
			}
			if g.PrBody != nil {
				mg.PRBody = *g.PrBody
			}
			if g.IssueTitle != nil {
				mg.IssueTitle = *g.IssueTitle
			}
			if g.IssueBody != nil {
				mg.IssueBody = *g.IssueBody
			}
			if g.IssueLabels != nil {
				mg.IssueLabels = *g.IssueLabels
			}
			if g.IssueAssignees != nil {
				mg.IssueAssignees = *g.IssueAssignees
			}
			m.Domain.Git = mg
		}
		if action.Domain.Payment != nil {
			p := action.Domain.Payment
			mp := &model.PaymentAction{}
			if p.VendorName != nil {
				mp.VendorName = *p.VendorName
			}
			if p.Amount != nil {
				mp.Amount = model.Money{
					Amount:   int64(p.Amount.Amount),
					Currency: p.Amount.Currency,
				}
			}
			if p.MerchantCategory != nil {
				mp.MerchantCategory = *p.MerchantCategory
			}
			m.Domain.Payment = mp
		}
		if action.Domain.Email != nil {
			e := action.Domain.Email
			me := &model.EmailAction{}
			if e.Subject != nil {
				me.Subject = *e.Subject
			}
			if e.BodyText != nil {
				me.BodyText = *e.BodyText
			}
			if e.BodyHtml != nil {
				me.BodyHTML = *e.BodyHtml
			}
			if e.SendMode != nil {
				me.SendMode = string(*e.SendMode)
			}
			if e.ThreadRef != nil {
				me.ThreadRef = *e.ThreadRef
			}
			if e.To != nil {
				for _, r := range *e.To {
					mr := model.Recipient{Email: string(r.Email)}
					if r.Name != nil {
						mr.Name = *r.Name
					}
					me.To = append(me.To, mr)
				}
			}
			if e.Cc != nil {
				for _, r := range *e.Cc {
					mr := model.Recipient{Email: string(r.Email)}
					if r.Name != nil {
						mr.Name = *r.Name
					}
					me.CC = append(me.CC, mr)
				}
			}
			if e.Bcc != nil {
				for _, r := range *e.Bcc {
					mr := model.Recipient{Email: string(r.Email)}
					if r.Name != nil {
						mr.Name = *r.Name
					}
					me.BCC = append(me.BCC, mr)
				}
			}
			m.Domain.Email = me
		}
		if action.Domain.Calendar != nil {
			c := action.Domain.Calendar
			mc := &model.CalendarAction{}
			if c.Title != nil {
				mc.Title = *c.Title
			}
			if c.Description != nil {
				mc.Description = *c.Description
			}
			if c.Provider != nil {
				mc.Provider = string(*c.Provider)
			}
			if c.Timezone != nil {
				mc.Timezone = *c.Timezone
			}
			if c.Location != nil {
				mc.Location = *c.Location
			}
			if c.ConferenceType != nil {
				mc.ConferenceType = string(*c.ConferenceType)
			}
			if c.CalendarId != nil {
				mc.CalendarID = *c.CalendarId
			}
			if c.Visibility != nil {
				mc.Visibility = string(*c.Visibility)
			}
			if c.StartTime != nil {
				mc.StartTime = c.StartTime
			}
			if c.EndTime != nil {
				mc.EndTime = c.EndTime
			}
			if c.Attendees != nil {
				for _, a := range *c.Attendees {
					attendee := model.CalendarAttendee{Email: string(a.Email)}
					if a.Name != nil {
						attendee.Name = *a.Name
					}
					if a.Optional != nil {
						attendee.Optional = *a.Optional
					}
					mc.Attendees = append(mc.Attendees, attendee)
				}
			}
			m.Domain.Calendar = mc
		}
	}
	return m
}

func apiContextToModel(ctx *api.IntentContext) model.IntentContext {
	if ctx == nil {
		return model.IntentContext{}
	}
	m := model.IntentContext{}
	if ctx.Environment != nil {
		m.Environment = *ctx.Environment
	}
	if ctx.SourcePlatform != nil {
		m.SourcePlatform = *ctx.SourcePlatform
	}
	if ctx.SourceSessionId != nil {
		m.SourceSessionID = *ctx.SourceSessionId
	}
	if ctx.SourceTraceId != nil {
		m.SourceTraceID = *ctx.SourceTraceId
	}
	if ctx.IpAddress != nil {
		m.IPAddress = *ctx.IpAddress
	}
	if ctx.UserPresent != nil {
		m.UserPresent = *ctx.UserPresent
	}
	return m
}

func modelDecisionToAPI(d model.Decision) api.Decision {
	apiD := api.Decision{
		Disposition: api.DecisionDisposition(d.Disposition),
		RiskLevel:   api.RiskLevel(d.RiskLevel),
	}
	if d.DenialReason != "" {
		apiD.DenialReason = &d.DenialReason
	}
	if len(d.MatchedPolicies) > 0 {
		var matches []api.PolicyMatch
		for _, m := range d.MatchedPolicies {
			pm := api.PolicyMatch{
				PolicyId:      &m.PolicyID,
				PolicyVersion: &m.PolicyVersion,
				RuleId:        &m.RuleID,
			}
			if m.Explanation != "" {
				pm.Explanation = &m.Explanation
			}
			matches = append(matches, pm)
		}
		apiD.MatchedPolicies = &matches
	}
	return apiD
}

// userIDFromRequest extracts the authenticated user ID from the request context.
// Returns empty string if auth is not enabled or the request is unauthenticated.
func userIDFromRequest(r *http.Request) string {
	if c := auth.ClaimsFromContext(r.Context()); c != nil {
		return c.Subject
	}
	return ""
}

// enterpriseIDFromRequest extracts the enterprise ID from the request context.
func enterpriseIDFromRequest(r *http.Request) string {
	if c := auth.ClaimsFromContext(r.Context()); c != nil {
		return c.EnterpriseID
	}
	return ""
}

// isAdmin returns true if the authenticated user has owner or admin role.
func isAdmin(r *http.Request) bool {
	c := auth.ClaimsFromContext(r.Context())
	if c == nil {
		return false
	}
	return c.Role == string(model.UserRoleOwner) || c.Role == string(model.UserRoleAdmin)
}

// requireAuth writes a 401 response if the user is not authenticated and returns false.
// When auth is disabled (no claims in context), it returns true with empty userID to allow
// unauthenticated access for local development.
func (s *apiServer) requireAuth(w http.ResponseWriter, r *http.Request) (userID, enterpriseID string, ok bool) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims != nil {
		return claims.Subject, claims.EnterpriseID, true
	}
	// Auth is disabled (local dev) — allow unauthenticated access.
	if s.users == nil {
		return "", "", true
	}
	writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
	return "", "", false
}
