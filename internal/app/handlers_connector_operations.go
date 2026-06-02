package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/sandbox/discovery"
)

const connectorOperationNotImplementedMessage = "connector operation dispatch is not implemented yet; mediated HTTPS data-plane execution is tracked by issue #896"
const sandboxProxyNotImplementedMessage = "sandbox HTTPS proxy data-plane execution is not implemented yet; credential injection at the proxy boundary is tracked by issue #896"

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
		Status:       api.ConnectorOperationRunRejectedResponseStatusRejected,
		AuditId:      auditID,
		ConnectorFqn: connectorFQN,
		Tool:         toolName,
		Operation:    operation.Name,
		Message:      connectorOperationNotImplementedMessage,
	})
}

func (s *apiServer) RecordSandboxProxyRequest(w http.ResponseWriter, r *http.Request) {
	var req api.SandboxProxyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid JSON request body")
		return
	}

	connectorFQN := strings.TrimSpace(req.ConnectorFqn)
	toolName := strings.TrimSpace(req.Tool)
	operationName := strings.TrimSpace(req.Operation)
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	upstreamURL := strings.TrimSpace(req.UpstreamUrl)
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
	case method == "":
		writeError(w, http.StatusBadRequest, "invalid_body", "method field is required")
		return
	case upstreamURL == "":
		writeError(w, http.StatusBadRequest, "invalid_body", "upstream_url field is required")
		return
	}
	upstream, err := parseSandboxProxyUpstreamURL(upstreamURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
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
	if !ok || !sandboxProxyRequestMatchesOperation(method, upstream, operation) {
		writeError(w, http.StatusNotFound, "sandbox_proxy_operation_not_found", "sandbox proxy operation not found")
		return
	}

	auditID := s.recordSandboxProxyRejected(r, connectorFQN, toolName, method, upstream, operation)
	writeJSON(w, http.StatusNotImplemented, api.SandboxProxyRejectedResponse{
		Status:       api.SandboxProxyRejectedResponseStatusRejected,
		AuditId:      auditID,
		ConnectorFqn: connectorFQN,
		Tool:         toolName,
		Operation:    operation.Name,
		Message:      sandboxProxyNotImplementedMessage,
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

func parseSandboxProxyUpstreamURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errInvalidSandboxProxyURL
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return nil, errSandboxProxyRequiresHTTPS
	}
	return parsed, nil
}

var (
	errInvalidSandboxProxyURL    = errors.New("upstream_url must be an absolute HTTPS URL")
	errSandboxProxyRequiresHTTPS = errors.New("upstream_url must use https")
)

func sandboxProxyRequestMatchesOperation(method string, upstream *url.URL, operation discovery.SpecOperationHelp) bool {
	if !strings.EqualFold(method, operation.Method) {
		return false
	}
	path := upstream.EscapedPath()
	if path == "" {
		path = "/"
	}
	return path == operation.Path
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

func (s *apiServer) recordSandboxProxyRejected(r *http.Request, connectorFQN, toolName, method string, upstream *url.URL, operation discovery.SpecOperationHelp) string {
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
		"aileron.connector.boundary":      "https_proxy",
		"aileron.connector.mediation":     "https_proxy",
		"aileron.connector.decision":      "rejected",
		"aileron.connector.reject_reason": "data_plane_not_implemented",
		"aileron.proxy.method":            method,
		"aileron.proxy.upstream.scheme":   upstream.Scheme,
		"aileron.proxy.upstream.host":     upstream.Host,
		"aileron.proxy.upstream.path":     upstream.EscapedPath(),
	}
	if operation.Credential != "" {
		payload["aileron.connector.credential_required"] = true
	}
	if sessionID := strings.TrimSpace(r.Header.Get("X-Aileron-Session-Id")); sessionID != "" {
		payload["aileron.session.id"] = sessionID
	}
	return s.auditRecorder.RecordSuccess(
		r.Context(),
		model.EventTypeConnectorProxyRejected,
		model.ActorRef{Type: model.ActorTypeAgent, ID: "sandbox-proxy"},
		payload,
	)
}
