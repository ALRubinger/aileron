package app

import (
	"encoding/json"
	"net/http"
	"strings"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/sandbox/discovery"
)

const connectorOperationNotImplementedMessage = "connector operation dispatch is not implemented yet; mediated HTTPS data-plane execution is tracked by issue #896"

func (s *apiServer) RunConnectorOperation(w http.ResponseWriter, r *http.Request) {
	var req api.ConnectorOperationRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid JSON request body")
		return
	}

	connectorFQN := strings.TrimSpace(req.ConnectorFqn)
	toolName := strings.TrimSpace(req.Tool)
	operationName := strings.TrimSpace(req.Operation)
	switch {
	case connectorFQN == "":
		writeError(w, http.StatusBadRequest, "invalid_body", "connector_fqn field is required")
		return
	case toolName == "":
		writeError(w, http.StatusBadRequest, "invalid_body", "tool field is required")
		return
	case operationName == "":
		writeError(w, http.StatusBadRequest, "invalid_body", "operation field is required")
		return
	}

	specs, err := s.loadConnectorOperationSpecs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "connector_specs_unavailable", err.Error())
		return
	}
	operation, ok, err := findSpecConnectorOperation(specs, connectorFQN, toolName, operationName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "connector_specs_invalid", err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "connector_operation_not_found", "connector operation not found")
		return
	}

	auditID := s.recordConnectorOperationRejected(r, connectorFQN, toolName, operation)
	writeJSON(w, http.StatusNotImplemented, api.ConnectorOperationRunRejectedResponse{
		Status:       api.Rejected,
		AuditId:      auditID,
		ConnectorFqn: connectorFQN,
		Tool:         toolName,
		Operation:    operation.Name,
		Message:      connectorOperationNotImplementedMessage,
	})
}

func (s *apiServer) loadConnectorOperationSpecs() ([]connectorspec.Spec, error) {
	if s.specLoader != nil {
		return s.specLoader()
	}
	return connectorspec.LoadInstalled("")
}

func findSpecConnectorOperation(specs []connectorspec.Spec, connectorFQN, toolName, operationName string) (discovery.SpecOperationHelp, bool, error) {
	tools, err := discovery.SpecConnectorTools(specs)
	if err != nil {
		return discovery.SpecOperationHelp{}, false, err
	}
	for _, tool := range tools {
		if tool.FQN != connectorFQN || tool.Name != toolName {
			continue
		}
		for _, operation := range tool.Operations {
			if operation.Name == operationName {
				return operation, true, nil
			}
		}
	}
	return discovery.SpecOperationHelp{}, false, nil
}

func (s *apiServer) recordConnectorOperationRejected(r *http.Request, connectorFQN, toolName string, operation discovery.SpecOperationHelp) string {
	if s.auditRecorder == nil {
		if s.newID != nil {
			return s.newID()
		}
		return audit.DefaultIDFn()
	}
	payload := map[string]any{
		"aileron.connector.fqn":           connectorFQN,
		"aileron.connector.tool":          toolName,
		"aileron.connector.operation":     operation.Name,
		"aileron.connector.method":        operation.Method,
		"aileron.connector.path":          operation.Path,
		"aileron.connector.idempotency":   operation.Idempotency,
		"aileron.connector.approval":      operation.Approval,
		"aileron.connector.credential":    operation.Credential,
		"aileron.connector.boundary":      "daemon_operation_endpoint",
		"aileron.connector.mediation":     "direct_shim_contract",
		"aileron.connector.decision":      "rejected",
		"aileron.connector.reject_reason": "data_plane_not_implemented",
	}
	if operation.Credential != "" {
		payload["aileron.connector.credential_required"] = true
	}
	if sessionID := strings.TrimSpace(r.Header.Get("X-Aileron-Session-Id")); sessionID != "" {
		payload["aileron.session.id"] = sessionID
	}
	return s.auditRecorder.RecordSuccess(
		r.Context(),
		model.EventTypeConnectorOperationRejected,
		model.ActorRef{Type: model.ActorTypeAgent, ID: "sandbox-shim"},
		payload,
	)
}
