package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/approval"
	"github.com/ALRubinger/aileron/core/auth"
	connectorpkg "github.com/ALRubinger/aileron/core/connector"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/notify"
	"github.com/ALRubinger/aileron/core/policy"
	"github.com/ALRubinger/aileron/core/registry"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/vault"
	"github.com/ALRubinger/aileron/core/version"
)

// apiServer implements the generated api.ServerInterface.
type apiServer struct {
	log            *slog.Logger
	registry       *connectorpkg.Registry
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
	mcpServers           *mem.MCPServerStore
	enterpriseMCPServers *mem.EnterpriseMCPServerStore
	registryClient       *registry.Client
	credentials    *mem.CredentialStore
	fundingSources *mem.FundingSourceStore
	traces         *mem.TraceStore
	enterprises    store.EnterpriseStore // nil when auth is disabled
	users          store.UserStore       // nil when auth is disabled
	newID          func() string
}

// --- JSON helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
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
		grant := api.ExecutionGrant{
			GrantId:   grantID,
			IntentId:  intentID,
			Status:    api.ExecutionGrantStatusActive,
			ExpiresAt: now.Add(5 * time.Minute),
		}
		s.grants.Create(ctx, grant)
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
	grant := api.ExecutionGrant{
		GrantId:   grantID,
		IntentId:  apr.IntentID,
		Status:    api.ExecutionGrantStatusActive,
		ExpiresAt: time.Now().UTC().Add(5 * time.Minute),
	}
	s.grants.Create(ctx, grant)

	// Update intent status.
	if intent, err := s.intents.Get(ctx, apr.IntentID); err == nil {
		intent.Status = api.Approved
		intent.Decision.ExecutionGrantId = &grantID
		intent.UpdatedAt = time.Now().UTC()
		s.intents.Update(ctx, intent)
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
	grant := api.ExecutionGrant{
		GrantId:           grantID,
		IntentId:          apr.IntentID,
		Status:            api.ExecutionGrantStatusActive,
		ExpiresAt:         time.Now().UTC().Add(5 * time.Minute),
		BoundedParameters: &req.Modifications,
	}
	s.grants.Create(ctx, grant)

	if intent, err := s.intents.Get(ctx, apr.IntentID); err == nil {
		intent.Status = api.Approved
		intent.Decision.ExecutionGrantId = &grantID
		intent.UpdatedAt = time.Now().UTC()
		s.intents.Update(ctx, intent)
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

	// Resolve credential from vault.
	vaultPath := fmt.Sprintf("connectors/%s/default", connProvider)
	secret, err := s.vault.Get(ctx, vaultPath)
	if err != nil {
		finishExecution(s, ctx, exec, intent, api.ExecutionStatusFailed, nil, "", "credential not found: "+vaultPath)
		writeError(w, http.StatusInternalServerError, "vault_error", "failed to resolve credential")
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

	result, err := conn.Execute(ctx, connectorpkg.ExecutionRequest{
		GrantID:    grant.GrantId,
		IntentID:   grant.IntentId,
		ActionType: intent.Action.Type,
		Parameters: params,
		Credential: &connectorpkg.InjectedCredential{
			Type:  secret.Metadata.Type,
			Value: secret.Value,
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
		Status:      api.ExecutionRunResponseStatusAccepted,
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

func resolveConnector(actionType string) (connType, provider string) {
	switch {
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
		if action.Domain.Calendar != nil {
			c := action.Domain.Calendar
			mc := &model.CalendarAction{}
			if c.Title != nil {
				mc.Title = *c.Title
			}
			if c.Attendees != nil {
				for _, a := range *c.Attendees {
					attendee := model.CalendarAttendee{Email: string(a.Email)}
					if a.Name != nil {
						attendee.Name = *a.Name
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

// --- MCP Server management ---

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

func (s *apiServer) ListMCPServers(w http.ResponseWriter, r *http.Request) {
	userID, enterpriseID, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	ctx := r.Context()

	// Fetch user's personal servers.
	filter := store.MCPServerFilter{}
	if userID != "" {
		filter.UserID = userID
	}
	servers, err := s.mcpServers.List(ctx, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// Tag personal servers with source.
	personal := api.MCPServerConfigSourcePersonal
	for i := range servers {
		servers[i].Source = &personal
	}

	// Merge enterprise auto-enabled servers.
	if enterpriseID != "" {
		autoEnabled := true
		entServers, err := s.enterpriseMCPServers.List(ctx, store.EnterpriseMCPServerFilter{
			EnterpriseID: enterpriseID,
			AutoEnabled:  &autoEnabled,
		})
		if err == nil {
			enterprise := api.MCPServerConfigSourceEnterprise
			for _, es := range entServers {
				servers = append(servers, enterpriseMCPToUserView(es, enterprise))
			}
		}
	}

	if servers == nil {
		servers = []api.MCPServerConfig{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": servers})
}

// enterpriseMCPToUserView converts an EnterpriseMCPServer to MCPServerConfig for the
// merged user server list.
func enterpriseMCPToUserView(es api.EnterpriseMCPServer, source api.MCPServerConfigSource) api.MCPServerConfig {
	cfg := api.MCPServerConfig{
		Id:          es.Id,
		Name:        es.Name,
		Description: es.Description,
		Command:     es.Command,
		Env:         es.Env,
		Version:     es.Version,
		RegistryId:  es.RegistryId,
		Source:      &source,
		CreatedAt:   es.CreatedAt,
		UpdatedAt:   es.UpdatedAt,
	}
	if es.Mode != nil {
		mode := api.MCPServerConfigMode(*es.Mode)
		cfg.Mode = &mode
	}
	if es.PolicyMapping != nil {
		cfg.PolicyMapping = &struct {
			ToolPrefix *string `json:"tool_prefix,omitempty"`
		}{
			ToolPrefix: es.PolicyMapping.ToolPrefix,
		}
	}
	stopped := api.MCPServerConfigStatusStopped
	cfg.Status = &stopped
	return cfg
}

func (s *apiServer) CreateMCPServer(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	var req api.MCPServerConfig
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	id := "mcp_" + s.newID()
	req.Id = &id
	if userID != "" {
		req.UserId = &userID
	}
	now := time.Now().UTC()
	req.CreatedAt = &now
	stopped := api.MCPServerConfigStatusStopped
	req.Status = &stopped
	personal := api.MCPServerConfigSourcePersonal
	req.Source = &personal
	if req.Mode == nil {
		local := api.MCPServerConfigModeLocal
		req.Mode = &local
	}

	if err := s.mcpServers.Create(r.Context(), req); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, req)
}

func (s *apiServer) GetMCPServer(w http.ResponseWriter, r *http.Request, id string) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	srv, err := s.mcpServers.Get(r.Context(), id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// Verify ownership (skip when auth is disabled).
	if userID != "" && srv.UserId != nil && *srv.UserId != userID {
		writeError(w, http.StatusNotFound, "not_found", "mcp_server not found: "+id)
		return
	}

	personal := api.MCPServerConfigSourcePersonal
	srv.Source = &personal
	writeJSON(w, http.StatusOK, srv)
}

func (s *apiServer) UpdateMCPServer(w http.ResponseWriter, r *http.Request, id string) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	var req api.MCPServerConfig
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	existing, err := s.mcpServers.Get(r.Context(), id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// Verify ownership.
	if userID != "" && existing.UserId != nil && *existing.UserId != userID {
		writeError(w, http.StatusNotFound, "not_found", "mcp_server not found: "+id)
		return
	}

	// Preserve read-only fields from existing record.
	req.Id = existing.Id
	req.UserId = existing.UserId
	req.Source = existing.Source
	req.CreatedAt = existing.CreatedAt
	req.Status = existing.Status
	now := time.Now().UTC()
	req.UpdatedAt = &now

	if err := s.mcpServers.Update(r.Context(), req); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, req)
}

func (s *apiServer) DeleteMCPServer(w http.ResponseWriter, r *http.Request, id string) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	existing, err := s.mcpServers.Get(r.Context(), id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// Verify ownership.
	if userID != "" && existing.UserId != nil && *existing.UserId != userID {
		writeError(w, http.StatusNotFound, "not_found", "mcp_server not found: "+id)
		return
	}

	if err := s.mcpServers.Delete(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- MCP Server credentials ---

func (s *apiServer) SetMCPServerCredential(w http.ResponseWriter, r *http.Request, id string) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	var req api.SetCredentialRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	ctx := r.Context()

	// Verify server exists and user owns it.
	srv, err := s.mcpServers.Get(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	if userID != "" && srv.UserId != nil && *srv.UserId != userID {
		writeError(w, http.StatusNotFound, "not_found", "mcp_server not found: "+id)
		return
	}

	// Store secret in vault.
	vaultPath := "mcp-servers/" + id + "/" + req.EnvVarName
	s.vault.Put(ctx, vaultPath, []byte(req.SecretValue), vault.Metadata{
		Type: "mcp_server_credential",
		Labels: map[string]string{
			"server_id": id,
			"env_var":   req.EnvVarName,
		},
	})

	// Update server config env to reference the vault path.
	envMap := make(map[string]string)
	if srv.Env != nil {
		for k, v := range *srv.Env {
			envMap[k] = v
		}
	}
	envMap[req.EnvVarName] = "vault://" + vaultPath
	srv.Env = &envMap
	now := time.Now().UTC()
	srv.UpdatedAt = &now

	if err := s.mcpServers.Update(ctx, srv); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, api.SetCredentialResponse{
		EnvVarName: req.EnvVarName,
		Stored:     true,
	})
}

// --- Enterprise MCP Server management ---

func (s *apiServer) ListEnterpriseMCPServers(w http.ResponseWriter, r *http.Request) {
	_, enterpriseID, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	filter := store.EnterpriseMCPServerFilter{}
	if enterpriseID != "" {
		filter.EnterpriseID = enterpriseID
	}
	servers, err := s.enterpriseMCPServers.List(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if servers == nil {
		servers = []api.EnterpriseMCPServer{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": servers})
}

func (s *apiServer) CreateEnterpriseMCPServer(w http.ResponseWriter, r *http.Request) {
	_, enterpriseID, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	if !isAdmin(r) {
		writeError(w, http.StatusForbidden, "forbidden", "owner or admin role required")
		return
	}

	var req api.EnterpriseMCPServer
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	id := "emcp_" + s.newID()
	req.Id = &id
	if enterpriseID != "" {
		req.EnterpriseId = &enterpriseID
	}
	now := time.Now().UTC()
	req.CreatedAt = &now
	if req.AutoEnabled == nil {
		f := false
		req.AutoEnabled = &f
	}
	if req.Mode == nil {
		local := api.EnterpriseMCPServerModeLocal
		req.Mode = &local
	}

	if err := s.enterpriseMCPServers.Create(r.Context(), req); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, req)
}

func (s *apiServer) GetEnterpriseMCPServer(w http.ResponseWriter, r *http.Request, id string) {
	_, enterpriseID, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	srv, err := s.enterpriseMCPServers.Get(r.Context(), id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	if enterpriseID != "" && srv.EnterpriseId != nil && *srv.EnterpriseId != enterpriseID {
		writeError(w, http.StatusNotFound, "not_found", "enterprise_mcp_server not found: "+id)
		return
	}

	writeJSON(w, http.StatusOK, srv)
}

func (s *apiServer) UpdateEnterpriseMCPServer(w http.ResponseWriter, r *http.Request, id string) {
	_, enterpriseID, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	if !isAdmin(r) {
		writeError(w, http.StatusForbidden, "forbidden", "owner or admin role required")
		return
	}

	var req api.EnterpriseMCPServer
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	existing, err := s.enterpriseMCPServers.Get(r.Context(), id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	if enterpriseID != "" && existing.EnterpriseId != nil && *existing.EnterpriseId != enterpriseID {
		writeError(w, http.StatusNotFound, "not_found", "enterprise_mcp_server not found: "+id)
		return
	}

	// Preserve read-only fields.
	req.Id = existing.Id
	req.EnterpriseId = existing.EnterpriseId
	req.CreatedAt = existing.CreatedAt
	now := time.Now().UTC()
	req.UpdatedAt = &now

	if err := s.enterpriseMCPServers.Update(r.Context(), req); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, req)
}

func (s *apiServer) DeleteEnterpriseMCPServer(w http.ResponseWriter, r *http.Request, id string) {
	_, enterpriseID, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	if !isAdmin(r) {
		writeError(w, http.StatusForbidden, "forbidden", "owner or admin role required")
		return
	}

	existing, err := s.enterpriseMCPServers.Get(r.Context(), id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	if enterpriseID != "" && existing.EnterpriseId != nil && *existing.EnterpriseId != enterpriseID {
		writeError(w, http.StatusNotFound, "not_found", "enterprise_mcp_server not found: "+id)
		return
	}

	if err := s.enterpriseMCPServers.Delete(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *apiServer) SetEnterpriseMCPServerCredential(w http.ResponseWriter, r *http.Request, id string) {
	_, enterpriseID, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	if !isAdmin(r) {
		writeError(w, http.StatusForbidden, "forbidden", "owner or admin role required")
		return
	}

	var req api.SetCredentialRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	ctx := r.Context()

	srv, err := s.enterpriseMCPServers.Get(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	if enterpriseID != "" && srv.EnterpriseId != nil && *srv.EnterpriseId != enterpriseID {
		writeError(w, http.StatusNotFound, "not_found", "enterprise_mcp_server not found: "+id)
		return
	}

	vaultPath := "mcp-servers/enterprise/" + enterpriseID + "/" + id + "/" + req.EnvVarName
	s.vault.Put(ctx, vaultPath, []byte(req.SecretValue), vault.Metadata{
		Type: "enterprise_mcp_server_credential",
		Labels: map[string]string{
			"enterprise_id": enterpriseID,
			"server_id":     id,
			"env_var":       req.EnvVarName,
		},
	})

	envMap := make(map[string]string)
	if srv.Env != nil {
		for k, v := range *srv.Env {
			envMap[k] = v
		}
	}
	envMap[req.EnvVarName] = "vault://" + vaultPath
	srv.Env = &envMap
	now := time.Now().UTC()
	srv.UpdatedAt = &now

	if err := s.enterpriseMCPServers.Update(ctx, srv); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, api.SetCredentialResponse{
		EnvVarName: req.EnvVarName,
		Stored:     true,
	})
}

// --- Marketplace ---

func (s *apiServer) ListMarketplaceServers(w http.ResponseWriter, r *http.Request, params api.ListMarketplaceServersParams) {
	userID, enterpriseID, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	ctx := r.Context()

	var query string
	if params.Q != nil {
		query = *params.Q
	}

	servers, err := s.registryClient.Search(ctx, query)
	if err != nil {
		writeError(w, http.StatusBadGateway, "registry_error", err.Error())
		return
	}

	// Build a set of installed registry IDs for enrichment (user's personal + enterprise auto-enabled).
	mcpFilter := store.MCPServerFilter{}
	if userID != "" {
		mcpFilter.UserID = userID
	}
	installed, _ := s.mcpServers.List(ctx, mcpFilter)
	installedSet := make(map[string]bool, len(installed))
	for _, srv := range installed {
		if srv.RegistryId != nil {
			installedSet[*srv.RegistryId] = true
		}
	}
	if enterpriseID != "" {
		autoEnabled := true
		entServers, _ := s.enterpriseMCPServers.List(ctx, store.EnterpriseMCPServerFilter{
			EnterpriseID: enterpriseID,
			AutoEnabled:  &autoEnabled,
		})
		for _, srv := range entServers {
			if srv.RegistryId != nil {
				installedSet[*srv.RegistryId] = true
			}
		}
	}

	// Group registry entries by name, collecting versions. The registry
	// returns entries in release order, so we preserve that ordering and
	// then reverse to get most-recent-first.
	type grouped struct {
		order       int
		description string
		versions    []api.MarketplaceServerVersion
	}
	byName := make(map[string]*grouped)
	var insertOrder int
	for _, srv := range servers {
		ver := api.MarketplaceServerVersion{
			Version: srv.Version,
		}
		var envVars []api.RequiredEnvVar
		for _, pkg := range srv.Packages {
			for _, ev := range pkg.EnvVars {
				envVars = append(envVars, api.RequiredEnvVar{
					Name:        ev.Name,
					Description: &ev.Description,
					Required:    &ev.Required,
				})
			}
		}
		if len(envVars) > 0 {
			ver.RequiredEnvVars = &envVars
		}

		if g, ok := byName[srv.Name]; ok {
			g.versions = append(g.versions, ver)
			// Keep the most recent description (last entry).
			if srv.Description != "" {
				g.description = srv.Description
			}
		} else {
			byName[srv.Name] = &grouped{
				order:       insertOrder,
				description: srv.Description,
				versions:    []api.MarketplaceServerVersion{ver},
			}
			insertOrder++
		}
	}

	// Build response items sorted by first-seen order.
	items := make([]api.MarketplaceServer, 0, len(byName))
	for name, g := range byName {
		// Reverse versions so most recent is first.
		for i, j := 0, len(g.versions)-1; i < j; i, j = i+1, j-1 {
			g.versions[i], g.versions[j] = g.versions[j], g.versions[i]
		}
		ms := api.MarketplaceServer{
			RegistryId: name,
			Name:       name,
		}
		if g.description != "" {
			ms.Description = &g.description
		}
		isInstalled := installedSet[name]
		ms.Installed = &isInstalled
		ms.Versions = &g.versions
		items = append(items, ms)
	}
	// Stable sort by insertion order.
	sort.Slice(items, func(i, j int) bool {
		return byName[items[i].Name].order < byName[items[j].Name].order
	})

	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *apiServer) InstallMarketplaceServer(w http.ResponseWriter, r *http.Request, registryId string) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	ctx := r.Context()

	srv, err := s.registryClient.Get(ctx, registryId)
	if err != nil {
		writeError(w, http.StatusBadGateway, "registry_error", err.Error())
		return
	}
	if srv == nil {
		writeError(w, http.StatusNotFound, "not_found", "server not found in registry: "+registryId)
		return
	}

	// Derive command from the best available package.
	command, envVars := deriveCommand(srv)

	id := "mcp_" + s.newID()
	now := time.Now().UTC()
	stopped := api.MCPServerConfigStatusStopped
	local := api.MCPServerConfigModeLocal
	personal := api.MCPServerConfigSourcePersonal
	config := api.MCPServerConfig{
		Id:         &id,
		Name:       srv.Name,
		Command:    command,
		Status:     &stopped,
		Mode:       &local,
		Source:     &personal,
		RegistryId: &registryId,
		CreatedAt:  &now,
	}
	if userID != "" {
		config.UserId = &userID
	}
	if srv.Description != "" {
		config.Description = &srv.Description
	}
	if srv.Version != "" {
		config.Version = &srv.Version
	}

	// Pre-populate env map with empty values for required env vars.
	if len(envVars) > 0 {
		envMap := make(map[string]string, len(envVars))
		for _, ev := range envVars {
			envMap[ev.Name] = ""
		}
		config.Env = &envMap
	}

	if err := s.mcpServers.Create(ctx, config); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// Build required credentials list.
	var reqCreds []api.RequiredEnvVar
	for _, ev := range envVars {
		reqCreds = append(reqCreds, api.RequiredEnvVar{
			Name:        ev.Name,
			Description: &ev.Description,
			Required:    &ev.Required,
		})
	}

	result := api.InstallResult{
		Server: config,
	}
	if len(reqCreds) > 0 {
		result.RequiredCredentials = &reqCreds
	}

	writeJSON(w, http.StatusCreated, result)
}

// deriveCommand picks the best package from a registry server and returns
// the command array and required env vars.
func deriveCommand(srv *registry.RegistryServer) ([]string, []registry.EnvVar) {
	// Prefer npm packages (most common for MCP servers).
	for _, pkg := range srv.Packages {
		if pkg.RegistryType == "npm" {
			cmd := []string{"npx", "-y", pkg.Name}
			if pkg.Runtime.Args != nil {
				cmd = append(cmd, pkg.Runtime.Args...)
			}
			return cmd, pkg.EnvVars
		}
	}

	// Fall back to any package with a runtime command.
	for _, pkg := range srv.Packages {
		if pkg.Runtime.Command != "" {
			cmd := []string{pkg.Runtime.Command}
			cmd = append(cmd, pkg.Runtime.Args...)
			return cmd, pkg.EnvVars
		}
	}

	// Last resort: use the server name as the command.
	return []string{srv.Name}, nil
}
