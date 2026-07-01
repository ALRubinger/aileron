package app

import (
	"bytes"
	"context"
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
	"github.com/ALRubinger/aileron/internal/binding"
	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
	"github.com/ALRubinger/aileron/internal/credential"
	"github.com/ALRubinger/aileron/internal/credential/inject"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/sandbox/discovery"
	"github.com/ALRubinger/aileron/internal/vault"
)

const connectorOperationNotProxyableMessage = "connector operation is not proxyable through the daemon HTTPS data plane; the spec lacks a required field (method, hosts, path) or uses an unsupported HTTP method"
const sandboxProxyMaxResponseBytes = 4 << 20

const (
	sandboxProxySourceGeneratedConnectorShim = "generated_connector_shim"
	sandboxProxySourceDaemonRequestBoundary  = "daemon_request_boundary"
	sandboxProxySourceTransparentConnectTLS  = "transparent_connect_tls"
)

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

	proxyReq, canProxy, err := connectorOperationRunProxyRequest(req, operation)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if canProxy {
		result, ok := s.executeSandboxProxyRequest(w, r, sandboxProxySourceGeneratedConnectorShim, connectorFQN, toolName, proxyReq.method, proxyReq.upstream, operation, proxyReq.body, proxyReq.contentType)
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
		Message:      connectorOperationNotProxyableMessage,
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

	result, ok := s.executeSandboxProxyRequest(w, r, sandboxProxySourceDaemonRequestBoundary, connectorFQN, toolName, method, upstream, operation, nil, "")
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

type connectorOperationProxyRequest struct {
	method      string
	upstream    *url.URL
	body        []byte
	contentType string
}

func connectorOperationRunProxyRequest(req api.ConnectorOperationRunRequest, operation discovery.SpecOperationHelp) (connectorOperationProxyRequest, bool, error) {
	method := strings.ToUpper(strings.TrimSpace(operation.Method))
	path := strings.TrimSpace(operation.Path)
	if path == "" {
		path = "/"
	}
	if method == "" || len(operation.Hosts) == 0 {
		return connectorOperationProxyRequest{}, false, nil
	}
	if path[0] != '/' {
		return connectorOperationProxyRequest{}, false, nil
	}

	args := map[string]any{}
	if req.Args != nil {
		args = *req.Args
	}
	upstream, err := parseSandboxProxyUpstreamURL("https://" + strings.TrimSpace(operation.Hosts[0]) + path)
	if err != nil {
		return connectorOperationProxyRequest{}, false, nil
	}
	proxyReq := connectorOperationProxyRequest{
		method:   method,
		upstream: upstream,
	}

	switch method {
	case http.MethodGet, http.MethodDelete, http.MethodHead:
		if err := addConnectorOperationQueryArgs(upstream.Query(), args, upstream); err != nil {
			return connectorOperationProxyRequest{}, false, err
		}
		return proxyReq, true, nil
	case http.MethodPost, http.MethodPatch, http.MethodPut:
		body, err := json.Marshal(args)
		if err != nil {
			return connectorOperationProxyRequest{}, false, fmt.Errorf("args must be JSON-serializable for request-body proxy execution: %w", err)
		}
		proxyReq.body = body
		proxyReq.contentType = "application/json"
		return proxyReq, true, nil
	default:
		return connectorOperationProxyRequest{}, false, nil
	}
}

func addConnectorOperationQueryArgs(query url.Values, args map[string]any, upstream *url.URL) error {
	if len(args) > 0 {
		for key, value := range args {
			key = strings.TrimSpace(key)
			if key == "" {
				return fmt.Errorf("args contains an empty query parameter name")
			}
			if err := addConnectorOperationQueryArg(query, key, value); err != nil {
				return err
			}
		}
	}
	upstream.RawQuery = query.Encode()
	return nil
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

func (s *apiServer) executeSandboxProxyRequest(w http.ResponseWriter, r *http.Request, source, connectorFQN, toolName, method string, upstream *url.URL, operation discovery.SpecOperationHelp, requestBody []byte, contentType string) (api.SandboxProxyResponse, bool) {
	var reqBody io.Reader
	if requestBody != nil {
		reqBody = bytes.NewReader(requestBody)
	}
	req, err := http.NewRequestWithContext(r.Context(), method, upstream.String(), reqBody)
	if err != nil {
		auditID := s.recordSandboxProxyRejected(r, source, connectorFQN, toolName, method, upstream, operation, "invalid_upstream_request")
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
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	if operation.Credential != "" {
		if !s.injectSandboxProxyCredential(w, r, req, source, connectorFQN, toolName, method, upstream, operation) {
			return api.SandboxProxyResponse{}, false
		}
	}

	client := sandboxProxyHTTPClient(s.sandboxProxyClient)
	resp, err := client.Do(req)
	if err != nil {
		auditID := s.recordSandboxProxyRejected(r, source, connectorFQN, toolName, method, upstream, operation, "upstream_transport_failed")
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
		auditID := s.recordSandboxProxyRejected(r, source, connectorFQN, toolName, method, upstream, operation, "upstream_read_failed")
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
		auditID := s.recordSandboxProxyRejected(r, source, connectorFQN, toolName, method, upstream, operation, "upstream_response_too_large")
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

	auditID := s.recordSandboxProxyProxied(r, source, connectorFQN, toolName, method, upstream, operation, resp.StatusCode)
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

func (s *apiServer) injectSandboxProxyCredential(w http.ResponseWriter, r *http.Request, req *http.Request, source, connectorFQN, toolName, method string, upstream *url.URL, operation discovery.SpecOperationHelp) bool {
	if s.bindings == nil {
		auditID := s.recordSandboxProxyRejected(r, source, connectorFQN, toolName, method, upstream, operation, "binding_store_unavailable")
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
		auditID := s.recordSandboxProxyRejected(r, source, connectorFQN, toolName, method, upstream, operation, "binding_required")
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
		auditID := s.recordSandboxProxyRejected(r, source, connectorFQN, toolName, method, upstream, operation, reason)
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
		auditID := s.recordSandboxProxyRejected(r, source, connectorFQN, toolName, method, upstream, operation, "credential_kind_mismatch")
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
		auditID := s.recordSandboxProxyRejected(r, source, connectorFQN, toolName, method, upstream, operation, "unsupported_credential_kind")
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

// Stable reject reasons emitted when a host-binding match resolves to a
// credential the daemon cannot inject. Wire identifiers carried in the
// rejection audit payload (aileron.proxy.reject_reason) — never the
// credential bytes or the credential-ref.
const (
	hostBindingRejectLockedVault           = "binding_locked_vault"
	hostBindingRejectCredentialUnavailable = "binding_credential_unavailable"
	hostBindingRejectUnsupportedScheme     = "binding_unsupported_scheme"
)

// injectSandboxProxyHostBindingCredential resolves the credential a
// matched host binding points at (daemon-side, through the vault) and
// injects it onto the upstream request per the binding's scheme. It
// returns ok=true when the request is ready to dial upstream, or
// ok=false and a stable reject reason when resolution or injection
// fails closed.
//
// Resolution goes through credential.VaultResolver against the daemon's
// vault keyed by hb.CredentialRef, so a locked or absent vault yields a
// resolver error and the request fails closed (the caller writes a 5xx
// to the in-container client). No secret bytes are logged, echoed into
// the response body, or written into the audit payload.
//
// The scheme switch is the seam #1194 slots into: this PR implements
// the `bearer` path concretely (mirroring injectSandboxProxyCredential's
// Authorization: Bearer behavior) and rejects any other scheme as
// unsupported until #1194's scheme-keyed injector replaces the switch.
func (s *apiServer) injectSandboxProxyHostBindingCredential(ctx context.Context, req *http.Request, hb binding.HostBinding) (bool, string) {
	if s.vault == nil {
		return false, hostBindingRejectCredentialUnavailable
	}
	// The credential-ref's first segment is its kind (<kind>/<service>/<identity>).
	expectedKind, _, _ := strings.Cut(hb.CredentialRef, "/")
	resolver := &credential.VaultResolver{
		Vault:        s.vault,
		VaultPath:    hb.CredentialRef,
		ExpectedKind: expectedKind,
	}
	cred, err := resolver.Resolve(ctx)
	if err != nil {
		if errors.Is(err, vault.ErrCredentialUnavailable) {
			return false, hostBindingRejectLockedVault
		}
		return false, hostBindingRejectCredentialUnavailable
	}
	return applyHostBindingScheme(req, hb, cred)
}

// applyHostBindingScheme applies the named injection scheme to the
// upstream request using the resolved credential. It is the narrow seam
// over the #1194 scheme-keyed injector (internal/credential/inject):
// the binding-match wiring is untouched and the injector owns the wire
// shape of every scheme, so a header convention lives in exactly one
// place. All five members of binding.HostBindingSchemes (`bearer`,
// `basic`, `header-template`, `query-param`, and `sigv4-resign`) are
// injected, each carrying its non-secret params from the binding. A scheme
// the dispatch does not recognize still fails closed (no upstream dial
// with an un-injected request) rather than silently passing through.
//
// The resolved credential bytes flow only into [inject.Inject], which
// writes them solely onto the request header or query parameter the
// scheme defines. They are never copied into the returned reject reason
// or any audit payload.
func applyHostBindingScheme(req *http.Request, hb binding.HostBinding, cred credential.Credential) (bool, string) {
	scheme, params, ok := hostBindingInjectScheme(hb)
	if !ok {
		return false, hostBindingRejectUnsupportedScheme
	}
	if err := inject.Inject(req, scheme, cred.Value, params); err != nil {
		// A missing param or an unimplemented scheme fails closed; the
		// error never carries the secret (inject keeps it off every
		// observable surface) so it is safe to map to a stable reason.
		return false, hostBindingRejectUnsupportedScheme
	}
	return true, ""
}

// hostBindingInjectScheme maps a binding's scheme string and its
// non-secret params onto the closed [inject.Scheme] set. Every member of
// binding.HostBindingSchemes maps to a concrete injector. It returns
// ok=false for any scheme outside that set, so the caller fails closed
// instead of dialing upstream un-injected.
func hostBindingInjectScheme(hb binding.HostBinding) (inject.Scheme, inject.Params, bool) {
	switch hb.Scheme {
	case binding.SchemeBearer:
		return inject.SchemeBearer, inject.Params{}, true
	case binding.SchemeBasic:
		return inject.SchemeBasic, inject.Params{Username: hb.BasicUsername}, true
	case binding.SchemeHeaderTemplate:
		return inject.SchemeHeaderTemplate, inject.Params{HeaderName: hb.HeaderName, Template: hb.HeaderTemplate}, true
	case binding.SchemeQueryParam:
		return inject.SchemeQueryParam, inject.Params{ParamName: hb.QueryParamName}, true
	case binding.SchemeSigV4Resign:
		return inject.SchemeSigV4Resign, inject.Params{AccessKeyID: hb.AccessKeyID, Region: hb.Region, Service: hb.Service}, true
	default:
		return "", inject.Params{}, false
	}
}

// Stable reject reasons for the per-step trust-contract gate at the
// bundled-CLI egress injection point (#1735). Wire identifiers carried in
// the trust_denied audit payload (aileron.proxy.reject_reason) — never a
// credential byte, the credential-ref, or an AllowedHosts value.
const (
	trustRejectHostNotAllowed   = "trust_host_not_allowed"
	trustRejectEffectNotAllowed = "trust_effect_not_allowed"
)

// enforceHostBindingTrust gates egress on a matched host binding's declared
// per-step trust contract BEFORE any credential injection (#1735). It
// returns (reason, ok): ok=true means the request may proceed (the binding
// declares no contract, or the upstream host and method satisfy it);
// ok=false with a stable reason means the request must be denied and audited
// as a denial (never passed through).
//
// Empty-allowlist semantics: an empty AllowedHosts declares no host scope,
// so the request stays scoped only to the matched HostPattern exactly as
// before. The allowlist check is skipped entirely — it is NOT passed to
// sandboxProxyHostAllowed, which returns false for an empty allowlist
// (deny). Only a NON-empty AllowedHosts is evaluated, so every pre-existing
// binding remains unconstrained-by-contract and injects as before.
//
// Effect semantics: an empty Effect applies no method gate. A read effect
// admits only HTTP-safe methods (GET/HEAD/OPTIONS); a mutating method is
// denied. Every write-class effect (write/delete/spend/external-send) admits
// all methods: the proxy sees only method + host and cannot distinguish
// write from delete/spend/external-send on the wire, so finer effect
// narrowing is enforced upstream at the runtime action-call approval seam,
// not here.
func enforceHostBindingTrust(hb binding.HostBinding, upstream *url.URL, method string) (string, bool) {
	if len(hb.AllowedHosts) > 0 && !sandboxProxyHostAllowed(upstream, hb.AllowedHosts) {
		return trustRejectHostNotAllowed, false
	}
	if hb.Effect == binding.EffectRead && !isSafeHTTPMethod(method) {
		return trustRejectEffectNotAllowed, false
	}
	return "", true
}

// isSafeHTTPMethod reports whether method is one of the HTTP-safe,
// non-mutating methods (GET/HEAD/OPTIONS) a read-effect trust contract
// admits. Any other method is treated as mutating at this seam.
func isSafeHTTPMethod(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// addHostBindingTrustIdentity stamps the optional, non-secret trust-contract
// effect and audit-addressing identity triple onto a proxy audit payload
// when the matched binding declares them. Empty fields are omitted. It
// carries the declared effect and the plan/step/tool addressing only, never
// a credential byte, the credential-ref, or an AllowedHosts value.
func addHostBindingTrustIdentity(payload map[string]any, hb binding.HostBinding) {
	if hb.Effect != "" {
		payload["aileron.trust.effect"] = hb.Effect
	}
	if hb.PlanID != "" {
		payload["aileron.plan.id"] = hb.PlanID
	}
	if hb.StepID != "" {
		payload["aileron.step.id"] = hb.StepID
	}
	if hb.ToolName != "" {
		payload["aileron.tool.name"] = hb.ToolName
	}
}

// recordSandboxProxyBindingInjected emits sandbox.proxy.binding_injected
// for a request that matched a host binding and was re-issued upstream
// with the bound credential injected. The payload carries the matched
// host pattern, scheme, and upstream destination/status, plus the optional
// trust-contract effect and plan/step/tool identity triple when the binding
// declares them (#1735); it never carries the credential bytes, the
// credential-ref, or the AllowedHosts values. nil-recorder safe like its
// siblings.
func (s *apiServer) recordSandboxProxyBindingInjected(r *http.Request, source string, hb binding.HostBinding, upstream *url.URL, upstreamStatus int) string {
	if s.auditRecorder == nil {
		if s.newID != nil {
			return s.newID()
		}
		return audit.DefaultIDFn()
	}
	payload := map[string]any{
		"aileron.proxy.boundary":        "https_proxy",
		"aileron.proxy.mediation":       "https_proxy",
		"aileron.proxy.source":          source,
		"aileron.proxy.decision":        "binding_injected",
		"aileron.proxy.method":          r.Method,
		"aileron.proxy.binding.host":    hb.HostPattern,
		"aileron.proxy.binding.scheme":  hb.Scheme,
		"aileron.proxy.upstream.scheme": upstream.Scheme,
		"aileron.proxy.upstream.host":   upstream.Host,
		"aileron.proxy.upstream.path":   sandboxProxyUpstreamPath(upstream),
		"aileron.proxy.upstream.status": upstreamStatus,
	}
	addHostBindingTrustIdentity(payload, hb)
	if sessionID := strings.TrimSpace(r.Header.Get("X-Aileron-Session-Id")); sessionID != "" {
		payload["aileron.session.id"] = sessionID
	}
	return s.auditRecorder.RecordSuccess(
		r.Context(),
		model.EventTypeSandboxProxyBindingInjected,
		model.ActorRef{Type: model.ActorTypeAgent, ID: "sandbox-proxy"},
		payload,
	)
}

// recordSandboxProxyTrustDenied emits sandbox.proxy.trust_denied for a
// bundled-CLI egress request that matched a host binding carrying a declared
// per-step trust contract and failed the trust gate at the injection point
// (#1735). The request was denied BEFORE any credential was resolved or
// injected and returned a 403 to the in-container client; it did NOT fall
// through to passthrough. The payload mirrors the binding_injected shape
// (boundary/mediation/source/decision/method/binding.host/binding.scheme/
// upstream.*), adds the stable reject reason, the declared trust effect, and
// the optional plan/step/tool identity triple. It never carries the
// credential bytes, the credential-ref, the AllowedHosts values, or an
// upstream query string. nil-recorder safe like its siblings.
func (s *apiServer) recordSandboxProxyTrustDenied(r *http.Request, source string, hb binding.HostBinding, upstream *url.URL, reason string) string {
	if s.auditRecorder == nil {
		if s.newID != nil {
			return s.newID()
		}
		return audit.DefaultIDFn()
	}
	payload := map[string]any{
		"aileron.proxy.boundary":        "https_proxy",
		"aileron.proxy.mediation":       "https_proxy",
		"aileron.proxy.source":          source,
		"aileron.proxy.decision":        "trust_denied",
		"aileron.proxy.reject_reason":   reason,
		"aileron.proxy.method":          r.Method,
		"aileron.proxy.binding.host":    hb.HostPattern,
		"aileron.proxy.binding.scheme":  hb.Scheme,
		"aileron.proxy.upstream.scheme": upstream.Scheme,
		"aileron.proxy.upstream.host":   upstream.Host,
		"aileron.proxy.upstream.path":   sandboxProxyUpstreamPath(upstream),
	}
	addHostBindingTrustIdentity(payload, hb)
	if sessionID := strings.TrimSpace(r.Header.Get("X-Aileron-Session-Id")); sessionID != "" {
		payload["aileron.session.id"] = sessionID
	}
	return s.auditRecorder.RecordSuccess(
		r.Context(),
		model.EventTypeSandboxProxyTrustDenied,
		model.ActorRef{Type: model.ActorTypeAgent, ID: "sandbox-proxy"},
		payload,
	)
}

// recordSandboxProxyForeignTokenNotSwapped emits
// sandbox.proxy.foreign_token_not_swapped for a request on a
// sentinel-swap host binding that carried a foreign
// (non-sentinel) token. The proxy did not swap it: it forwarded the
// request unchanged with no real credential injected. The payload
// carries the matched host pattern, scheme, and upstream
// destination/status, plus the optional trust-contract effect and
// plan/step/tool identity triple when the binding declares them (#1735);
// it never carries the foreign token, the sentinel, the real credential,
// or the credential-ref. nil-recorder safe like its siblings.
func (s *apiServer) recordSandboxProxyForeignTokenNotSwapped(r *http.Request, source string, hb binding.HostBinding, upstream *url.URL, upstreamStatus int) string {
	if s.auditRecorder == nil {
		if s.newID != nil {
			return s.newID()
		}
		return audit.DefaultIDFn()
	}
	payload := map[string]any{
		"aileron.proxy.boundary":        "https_proxy",
		"aileron.proxy.mediation":       "https_proxy",
		"aileron.proxy.source":          source,
		"aileron.proxy.decision":        "foreign_token_not_swapped",
		"aileron.proxy.method":          r.Method,
		"aileron.proxy.binding.host":    hb.HostPattern,
		"aileron.proxy.binding.scheme":  hb.Scheme,
		"aileron.proxy.upstream.scheme": upstream.Scheme,
		"aileron.proxy.upstream.host":   upstream.Host,
		"aileron.proxy.upstream.path":   sandboxProxyUpstreamPath(upstream),
		"aileron.proxy.upstream.status": upstreamStatus,
	}
	addHostBindingTrustIdentity(payload, hb)
	if sessionID := strings.TrimSpace(r.Header.Get("X-Aileron-Session-Id")); sessionID != "" {
		payload["aileron.session.id"] = sessionID
	}
	return s.auditRecorder.RecordSuccess(
		r.Context(),
		model.EventTypeSandboxProxyForeignTokenNotSwapped,
		model.ActorRef{Type: model.ActorTypeAgent, ID: "sandbox-proxy"},
		payload,
	)
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
		"aileron.connector.reject_reason": "operation_not_proxyable",
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

func (s *apiServer) recordSandboxProxyRejected(r *http.Request, source, connectorFQN, toolName, method string, upstream *url.URL, operation discovery.SpecOperationHelp, reason string) string {
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
		"aileron.proxy.source":            source,
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

func (s *apiServer) recordSandboxProxyProxied(r *http.Request, source, connectorFQN, toolName, method string, upstream *url.URL, operation discovery.SpecOperationHelp, upstreamStatus int) string {
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
		"aileron.proxy.source":          source,
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

func (s *apiServer) recordSandboxProxyPassthrough(r *http.Request, source, method string, upstream *url.URL, upstreamStatus int) string {
	if s.auditRecorder == nil {
		if s.newID != nil {
			return s.newID()
		}
		return audit.DefaultIDFn()
	}
	payload := map[string]any{
		"aileron.proxy.boundary":        "https_proxy",
		"aileron.proxy.mediation":       "https_proxy",
		"aileron.proxy.source":          source,
		"aileron.proxy.decision":        "passthrough",
		"aileron.proxy.method":          method,
		"aileron.proxy.upstream.scheme": upstream.Scheme,
		"aileron.proxy.upstream.host":   upstream.Host,
		"aileron.proxy.upstream.path":   sandboxProxyUpstreamPath(upstream),
		"aileron.proxy.upstream.status": upstreamStatus,
	}
	if sessionID := strings.TrimSpace(r.Header.Get("X-Aileron-Session-Id")); sessionID != "" {
		payload["aileron.session.id"] = sessionID
	}
	return s.auditRecorder.RecordSuccess(
		r.Context(),
		model.EventTypeSandboxProxyPassthrough,
		model.ActorRef{Type: model.ActorTypeAgent, ID: "sandbox-proxy"},
		payload,
	)
}

// recordSandboxProxyUpgrade emits sandbox.proxy.upgrade for a WebSocket
// (or other HTTP Upgrade) handshake forwarded through the passthrough
// boundary. The upstream's handshake status is recorded; no credential
// is injected and the tunnel bytes never appear in the payload.
func (s *apiServer) recordSandboxProxyUpgrade(r *http.Request, source, method string, upstream *url.URL, upstreamStatus int) string {
	if s.auditRecorder == nil {
		if s.newID != nil {
			return s.newID()
		}
		return audit.DefaultIDFn()
	}
	payload := map[string]any{
		"aileron.proxy.boundary":        "https_proxy",
		"aileron.proxy.mediation":       "https_proxy",
		"aileron.proxy.source":          source,
		"aileron.proxy.decision":        "upgrade",
		"aileron.proxy.method":          method,
		"aileron.proxy.upstream.scheme": upstream.Scheme,
		"aileron.proxy.upstream.host":   upstream.Host,
		"aileron.proxy.upstream.path":   sandboxProxyUpstreamPath(upstream),
		"aileron.proxy.upstream.status": upstreamStatus,
	}
	if sessionID := strings.TrimSpace(r.Header.Get("X-Aileron-Session-Id")); sessionID != "" {
		payload["aileron.session.id"] = sessionID
	}
	return s.auditRecorder.RecordSuccess(
		r.Context(),
		model.EventTypeSandboxProxyUpgrade,
		model.ActorRef{Type: model.ActorTypeAgent, ID: "sandbox-proxy"},
		payload,
	)
}

// recordSandboxProxyProtocolRejected emits sandbox.proxy.rejected for
// protocol-level failures only: non-CONNECT proxy requests, session CA
// unavailable, connector specs invalid/unavailable, and passthrough
// upstream failures. Match-failure outcomes (no spec matched,
// ambiguous match) are passthrough under the credential-injection-only
// model and are recorded by recordSandboxProxyPassthrough instead.
func (s *apiServer) recordSandboxProxyProtocolRejected(r *http.Request, source, method string, upstream *url.URL, reason string) string {
	if s.auditRecorder == nil {
		if s.newID != nil {
			return s.newID()
		}
		return audit.DefaultIDFn()
	}
	payload := map[string]any{
		"aileron.proxy.boundary":        "https_proxy",
		"aileron.proxy.mediation":       "https_proxy",
		"aileron.proxy.decision":        "rejected",
		"aileron.proxy.reject_reason":   reason,
		"aileron.proxy.source":          source,
		"aileron.proxy.method":          method,
		"aileron.proxy.upstream.scheme": upstream.Scheme,
		"aileron.proxy.upstream.host":   upstream.Host,
		"aileron.proxy.upstream.path":   sandboxProxyUpstreamPath(upstream),
	}
	if sessionID := strings.TrimSpace(r.Header.Get("X-Aileron-Session-Id")); sessionID != "" {
		payload["aileron.session.id"] = sessionID
	}
	return s.auditRecorder.RecordSuccess(
		r.Context(),
		model.EventTypeSandboxProxyRejected,
		model.ActorRef{Type: model.ActorTypeAgent, ID: "sandbox-proxy"},
		payload,
	)
}
