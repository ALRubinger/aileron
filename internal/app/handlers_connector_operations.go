package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
	"github.com/ALRubinger/aileron/internal/credential"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/sandbox/discovery"
)

const connectorOperationNotImplementedMessage = "connector operation dispatch is not implemented yet; mediated HTTPS data-plane execution is tracked by issue #896"
const sandboxProxyMaxResponseBytes = 4 << 20

var sandboxProxyDefaultClient = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: sandboxProxyRejectRedirect,
}

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

	upstream, canProxy, err := connectorOperationRunUpstreamURL(req, operation)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if canProxy {
		result, ok := s.executeSandboxProxyRequest(w, r, connectorFQN, toolName, strings.ToUpper(operation.Method), upstream, operation)
		if !ok {
			return
		}
		writeJSON(w, http.StatusOK, result)
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

	result, ok := s.executeSandboxProxyRequest(w, r, connectorFQN, toolName, method, upstream, operation)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, result)
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

func connectorOperationRunUpstreamURL(req api.ConnectorOperationRunRequest, operation discovery.SpecOperationHelp) (*url.URL, bool, error) {
	method := strings.ToUpper(strings.TrimSpace(operation.Method))
	path := strings.TrimSpace(operation.Path)
	if path == "" {
		path = "/"
	}
	if method == "" || len(operation.Hosts) == 0 {
		return nil, false, nil
	}
	if path[0] != '/' {
		return nil, false, nil
	}
	switch method {
	case http.MethodGet, http.MethodDelete, http.MethodHead:
	default:
		return nil, false, nil
	}

	args := map[string]any{}
	if req.Args != nil {
		args = *req.Args
	}
	query := url.Values{}
	if len(args) > 0 {
		for key, value := range args {
			key = strings.TrimSpace(key)
			if key == "" {
				return nil, false, fmt.Errorf("args contains an empty query parameter name")
			}
			if err := addConnectorOperationQueryArg(query, key, value); err != nil {
				return nil, false, err
			}
		}
	}

	upstream, err := parseSandboxProxyUpstreamURL("https://" + strings.TrimSpace(operation.Hosts[0]) + path)
	if err != nil {
		return nil, false, nil
	}
	upstream.RawQuery = query.Encode()
	return upstream, true, nil
}

func addConnectorOperationQueryArg(query url.Values, key string, value any) error {
	switch value := value.(type) {
	case nil:
		query.Add(key, "")
	case string:
		query.Add(key, value)
	case bool:
		query.Add(key, fmt.Sprintf("%t", value))
	case float64:
		query.Add(key, fmt.Sprintf("%v", value))
	case []any:
		for _, item := range value {
			if err := addConnectorOperationQueryArg(query, key, item); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("args.%s must be a string, number, boolean, null, or array of those values for bodyless proxy execution", key)
	}
	return nil
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
	if sandboxProxyUpstreamPath(upstream) != operation.Path {
		return false
	}
	return sandboxProxyHostAllowed(upstream, operation.Hosts)
}

func sandboxProxyUpstreamPath(upstream *url.URL) string {
	path := upstream.EscapedPath()
	if path == "" {
		return "/"
	}
	return path
}

func sandboxProxyHostAllowed(upstream *url.URL, allowed []string) bool {
	if len(allowed) == 0 {
		return false
	}
	upstreamHost := strings.ToLower(upstream.Host)
	upstreamHostDefaultPort := sandboxProxyHostWithDefaultPort(upstream)
	for _, host := range allowed {
		host = strings.ToLower(strings.TrimSpace(host))
		if host == "" {
			continue
		}
		if host == upstreamHost || host == upstreamHostDefaultPort {
			return true
		}
		if !strings.Contains(host, ":") && host == strings.ToLower(upstream.Hostname()) && (upstream.Port() == "" || upstream.Port() == "443") {
			return true
		}
	}
	return false
}

func sandboxProxyHostWithDefaultPort(upstream *url.URL) string {
	if upstream.Port() != "" {
		return strings.ToLower(upstream.Host)
	}
	if strings.EqualFold(upstream.Scheme, "https") {
		return strings.ToLower(upstream.Hostname()) + ":443"
	}
	return strings.ToLower(upstream.Host)
}

func (s *apiServer) executeSandboxProxyRequest(w http.ResponseWriter, r *http.Request, connectorFQN, toolName, method string, upstream *url.URL, operation discovery.SpecOperationHelp) (api.SandboxProxyResponse, bool) {
	req, err := http.NewRequestWithContext(r.Context(), method, upstream.String(), nil)
	if err != nil {
		auditID := s.recordSandboxProxyRejected(r, connectorFQN, toolName, method, upstream, operation, "invalid_upstream_request")
		writeJSON(w, http.StatusBadRequest, api.SandboxProxyRejectedResponse{
			Status:       api.SandboxProxyRejectedResponseStatusRejected,
			AuditId:      auditID,
			ConnectorFqn: connectorFQN,
			Tool:         toolName,
			Operation:    operation.Name,
			Message:      "sandbox proxy upstream request is invalid",
		})
		return api.SandboxProxyResponse{}, false
	}
	req.Header.Set("User-Agent", "aileron-sandbox-proxy")

	if operation.Credential != "" {
		if !s.injectSandboxProxyCredential(w, r, req, connectorFQN, toolName, method, upstream, operation) {
			return api.SandboxProxyResponse{}, false
		}
	}

	client := sandboxProxyHTTPClient(s.sandboxProxyClient)
	resp, err := client.Do(req)
	if err != nil {
		auditID := s.recordSandboxProxyRejected(r, connectorFQN, toolName, method, upstream, operation, "upstream_transport_failed")
		writeJSON(w, http.StatusBadGateway, api.SandboxProxyRejectedResponse{
			Status:       api.SandboxProxyRejectedResponseStatusRejected,
			AuditId:      auditID,
			ConnectorFqn: connectorFQN,
			Tool:         toolName,
			Operation:    operation.Name,
			Message:      "sandbox proxy upstream request failed",
		})
		return api.SandboxProxyResponse{}, false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, sandboxProxyMaxResponseBytes+1))
	if err != nil {
		auditID := s.recordSandboxProxyRejected(r, connectorFQN, toolName, method, upstream, operation, "upstream_read_failed")
		writeJSON(w, http.StatusBadGateway, api.SandboxProxyRejectedResponse{
			Status:       api.SandboxProxyRejectedResponseStatusRejected,
			AuditId:      auditID,
			ConnectorFqn: connectorFQN,
			Tool:         toolName,
			Operation:    operation.Name,
			Message:      "sandbox proxy upstream response read failed",
		})
		return api.SandboxProxyResponse{}, false
	}
	if len(body) > sandboxProxyMaxResponseBytes {
		auditID := s.recordSandboxProxyRejected(r, connectorFQN, toolName, method, upstream, operation, "upstream_response_too_large")
		writeJSON(w, http.StatusBadGateway, api.SandboxProxyRejectedResponse{
			Status:       api.SandboxProxyRejectedResponseStatusRejected,
			AuditId:      auditID,
			ConnectorFqn: connectorFQN,
			Tool:         toolName,
			Operation:    operation.Name,
			Message:      "sandbox proxy upstream response exceeded size limit",
		})
		return api.SandboxProxyResponse{}, false
	}

	auditID := s.recordSandboxProxyProxied(r, connectorFQN, toolName, method, upstream, operation, resp.StatusCode)
	return api.SandboxProxyResponse{
		Status:         api.Proxied,
		AuditId:        auditID,
		ConnectorFqn:   connectorFQN,
		Tool:           toolName,
		Operation:      operation.Name,
		UpstreamStatus: resp.StatusCode,
		ContentType:    ptrNonEmpty(resp.Header.Get("Content-Type")),
		BodyBase64:     body,
	}, true
}

func sandboxProxyRejectRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func sandboxProxyHTTPClient(client *http.Client) *http.Client {
	if client == nil {
		return sandboxProxyDefaultClient
	}
	copy := *client
	if copy.Timeout == 0 {
		copy.Timeout = 30 * time.Second
	}
	copy.CheckRedirect = sandboxProxyRejectRedirect
	return &copy
}

func (s *apiServer) injectSandboxProxyCredential(w http.ResponseWriter, r *http.Request, req *http.Request, connectorFQN, toolName, method string, upstream *url.URL, operation discovery.SpecOperationHelp) bool {
	if s.bindings == nil {
		auditID := s.recordSandboxProxyRejected(r, connectorFQN, toolName, method, upstream, operation, "binding_store_unavailable")
		writeJSON(w, http.StatusForbidden, api.SandboxProxyRejectedResponse{
			Status:       api.SandboxProxyRejectedResponseStatusRejected,
			AuditId:      auditID,
			ConnectorFqn: connectorFQN,
			Tool:         toolName,
			Operation:    operation.Name,
			Message:      "sandbox proxy credential binding store is unavailable",
		})
		return false
	}
	resolver := s.bindings.ResolverFor(r.Context(), connectorFQN, operation.Credential)
	if resolver == nil {
		auditID := s.recordSandboxProxyRejected(r, connectorFQN, toolName, method, upstream, operation, "binding_required")
		writeJSON(w, http.StatusForbidden, api.SandboxProxyRejectedResponse{
			Status:       api.SandboxProxyRejectedResponseStatusRejected,
			AuditId:      auditID,
			ConnectorFqn: connectorFQN,
			Tool:         toolName,
			Operation:    operation.Name,
			Message:      "sandbox proxy credential binding is required",
		})
		return false
	}
	cred, err := resolver.Resolve(r.Context())
	if err != nil {
		reason := "credential_resolution_failed"
		status := http.StatusInternalServerError
		message := "sandbox proxy credential resolution failed"
		switch {
		case errors.Is(err, credential.ErrBindingMissing), errors.Is(err, credential.ErrNoBindingResolver):
			reason = "binding_required"
			status = http.StatusForbidden
			message = "sandbox proxy credential binding is required"
		case errors.Is(err, credential.ErrCredentialKindMismatch):
			reason = "credential_kind_mismatch"
			status = http.StatusForbidden
			message = "sandbox proxy credential kind does not match connector spec"
		}
		auditID := s.recordSandboxProxyRejected(r, connectorFQN, toolName, method, upstream, operation, reason)
		writeJSON(w, status, api.SandboxProxyRejectedResponse{
			Status:       api.SandboxProxyRejectedResponseStatusRejected,
			AuditId:      auditID,
			ConnectorFqn: connectorFQN,
			Tool:         toolName,
			Operation:    operation.Name,
			Message:      message,
		})
		return false
	}
	if cred.Kind != operation.Credential {
		auditID := s.recordSandboxProxyRejected(r, connectorFQN, toolName, method, upstream, operation, "credential_kind_mismatch")
		writeJSON(w, http.StatusForbidden, api.SandboxProxyRejectedResponse{
			Status:       api.SandboxProxyRejectedResponseStatusRejected,
			AuditId:      auditID,
			ConnectorFqn: connectorFQN,
			Tool:         toolName,
			Operation:    operation.Name,
			Message:      "sandbox proxy credential kind does not match connector spec",
		})
		return false
	}
	switch cred.Kind {
	case "oauth2", "api_key":
		req.Header.Set("Authorization", "Bearer "+string(cred.Value))
	default:
		auditID := s.recordSandboxProxyRejected(r, connectorFQN, toolName, method, upstream, operation, "unsupported_credential_kind")
		writeJSON(w, http.StatusForbidden, api.SandboxProxyRejectedResponse{
			Status:       api.SandboxProxyRejectedResponseStatusRejected,
			AuditId:      auditID,
			ConnectorFqn: connectorFQN,
			Tool:         toolName,
			Operation:    operation.Name,
			Message:      "sandbox proxy credential kind is not supported",
		})
		return false
	}
	return true
}

func ptrNonEmpty(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
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

func (s *apiServer) recordSandboxProxyRejected(r *http.Request, connectorFQN, toolName, method string, upstream *url.URL, operation discovery.SpecOperationHelp, reason string) string {
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
		"aileron.connector.reject_reason": reason,
		"aileron.proxy.method":            method,
		"aileron.proxy.upstream.scheme":   upstream.Scheme,
		"aileron.proxy.upstream.host":     upstream.Host,
		"aileron.proxy.upstream.path":     sandboxProxyUpstreamPath(upstream),
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

func (s *apiServer) recordSandboxProxyProxied(r *http.Request, connectorFQN, toolName, method string, upstream *url.URL, operation discovery.SpecOperationHelp, upstreamStatus int) string {
	if s.auditRecorder == nil {
		if s.newID != nil {
			return s.newID()
		}
		return audit.DefaultIDFn()
	}
	payload := map[string]any{
		"aileron.connector.fqn":         connectorFQN,
		"aileron.connector.tool":        toolName,
		"aileron.connector.operation":   operation.Name,
		"aileron.connector.method":      operation.Method,
		"aileron.connector.path":        operation.Path,
		"aileron.connector.idempotency": operation.Idempotency,
		"aileron.connector.approval":    operation.Approval,
		"aileron.connector.credential":  operation.Credential,
		"aileron.connector.boundary":    "https_proxy",
		"aileron.connector.mediation":   "https_proxy",
		"aileron.connector.decision":    "proxied",
		"aileron.proxy.method":          method,
		"aileron.proxy.upstream.scheme": upstream.Scheme,
		"aileron.proxy.upstream.host":   upstream.Host,
		"aileron.proxy.upstream.path":   sandboxProxyUpstreamPath(upstream),
		"aileron.proxy.upstream.status": upstreamStatus,
	}
	if operation.Credential != "" {
		payload["aileron.connector.credential_required"] = true
	}
	if sessionID := strings.TrimSpace(r.Header.Get("X-Aileron-Session-Id")); sessionID != "" {
		payload["aileron.session.id"] = sessionID
	}
	return s.auditRecorder.RecordSuccess(
		r.Context(),
		model.EventTypeConnectorProxyProxied,
		model.ActorRef{Type: model.ActorTypeAgent, ID: "sandbox-proxy"},
		payload,
	)
}
