package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/tetratelabs/wazero/api"

	"github.com/ALRubinger/aileron/internal/credential"
)

// HTTPDoer is the narrow HTTP-client surface the sandbox needs. It
// matches `*http.Client.Do`'s signature so tests can substitute an
// in-memory client without HTTP. The runtime injects a Doer per
// invocation; default is a stock `*http.Client`.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// hostModuleName is the name connectors import host functions under.
// Connector authors call `aileron_host.log(...)`, `aileron_host.http_request(...)`,
// etc. — see the package doc for the v1 ABI.
const hostModuleName = "aileron_host"

// hostState is the per-invocation state the host functions read and
// write. One instance lives for one [Connector.Invoke] call.
//
// The capability check is in `policy`: every `http_request` is
// validated against the connector manifest's network grant before any
// network call is made, satisfying ADR-0005 acceptance criterion #3.
//
// `actionGrant` (when non-nil and non-empty) is the action manifest's
// declared subset of capabilities — checked alongside the connector's
// network grant per ADR-0003 §"defense in depth" / issue #359 AC #7.
// v1 stores the action's allowed capability strings; the network
// import inspects them to decide whether the network capability falls
// within the action's declared subset.
type hostState struct {
	policy      *HostPolicy
	doer        HTTPDoer
	actionGrant map[string]struct{}
	logger      *slog.Logger

	// connectorFQN is the fully-qualified name of the connector being
	// invoked. Threaded into structured-error details so denials at the
	// sandbox boundary identify which connector emitted the request.
	connectorFQN string

	// expectedCredentialKind is the connector manifest's declared
	// `[capabilities.credential].kind`. Empty when the connector
	// declares no credential capability — any `credential` field on an
	// http_request envelope is then refused as capability_denied.
	expectedCredentialKind string

	// credentialHeader and credentialFormat are the manifest's optional
	// `[capabilities.credential].header` / `.format` fields, only
	// consulted for api_key credentials. Empty means the v1 defaults
	// (`Authorization: Bearer <key>`). The `{key}` placeholder in
	// credentialFormat is substituted with the resolved value at
	// injection time. OAuth2 ignores both fields — RFC 6750 fixes its
	// wire format.
	credentialHeader string
	credentialFormat string

	// credentialResolver is the per-Invoke handle the host uses to
	// fetch a bound credential when the connector's http_request
	// envelope carries a `credential` field. Nil when the action did
	// not declare a binding for this connector; the host returns
	// `binding_required` if the connector tries to use the path.
	credentialResolver credential.Resolver

	// spawnPolicy is the connector manifest's [capabilities.spawn]
	// declaration in enforced form (per ADR-0002 spawn primitive,
	// ADR-0014 sandbox tech). Nil when the connector did not declare
	// spawn — every spawn host-function call then refuses with
	// capability_denied.
	spawnPolicy *SpawnPolicy

	// spawnExecutor is the platform-specific subprocess executor.
	// Defaults to defaultSpawnExecutor when the runtime was built
	// without WithSpawnExecutor; tests inject fakes.
	spawnExecutor SpawnExecutor

	// actionSpawnGrant is the action manifest's declared subset of
	// programs the connector may spawn. Empty means the action did
	// not narrow the connector's grant. Non-empty entries act as the
	// second-line gate alongside the connector manifest's policy.
	actionSpawnGrant map[string]struct{}

	mu             sync.Mutex
	logs           []LogLine
	response       []byte
	respCode       int
	respErr        *Error // sticky: set when http_request itself was denied or failed
	spawnExitCode  int
	spawnStdout    []byte
	spawnStderr    []byte
	spawnErr       *Error // sticky: set when spawn was denied or failed
	spawnHasResult bool
}

// hostStateKey is the context key used to thread per-invocation state
// into the host functions. Using context (rather than closure capture)
// lets the host module be built once and reused across invocations —
// each call provides its own state via context.
type hostStateKey struct{}

func ctxWithState(ctx context.Context, s *hostState) context.Context {
	return context.WithValue(ctx, hostStateKey{}, s)
}

func stateFromCtx(ctx context.Context) *hostState {
	s, _ := ctx.Value(hostStateKey{}).(*hostState)
	return s
}

// readMemory copies `length` bytes starting at `offset` from the
// module's exported linear memory. Returns nil when the read is
// out-of-bounds — caller surfaces a structured connector_runtime_error.
func readMemory(mod api.Module, offset, length uint32) []byte {
	if length == 0 {
		return nil
	}
	mem := mod.Memory()
	if mem == nil {
		return nil
	}
	buf, ok := mem.Read(offset, length)
	if !ok {
		return nil
	}
	out := make([]byte, length)
	copy(out, buf)
	return out
}

// writeMemory writes data into the module's linear memory at offset.
// Returns the number of bytes written; zero on failure.
func writeMemory(mod api.Module, offset uint32, data []byte) uint32 {
	mem := mod.Memory()
	if mem == nil {
		return 0
	}
	if !mem.Write(offset, data) {
		return 0
	}
	return uint32(len(data))
}

// hostLog implements `aileron_host.log(level_ptr, level_len, msg_ptr, msg_len)`.
// Fire-and-forget; out-of-bounds reads are dropped silently rather than
// terminating the connector.
func hostLog(ctx context.Context, mod api.Module, levelPtr, levelLen, msgPtr, msgLen uint32) {
	s := stateFromCtx(ctx)
	if s == nil {
		return
	}
	level := string(readMemory(mod, levelPtr, levelLen))
	msg := string(readMemory(mod, msgPtr, msgLen))
	s.mu.Lock()
	s.logs = append(s.logs, LogLine{Level: level, Message: msg})
	s.mu.Unlock()
	if s.logger != nil {
		s.logger.LogAttrs(ctx, slog.LevelInfo, "connector log",
			slog.String("level", level),
			slog.String("message", msg),
		)
	}
}

// httpRequestEnvelope is the JSON shape connectors marshal as the input
// to `aileron_host.http_request`. Designed to be ergonomic across
// language ABIs; the runtime parses it host-side.
//
// `Credential` (optional) names the kind of credential the connector
// wants the runtime to attach to this request. Per ADR-0005 credential
// mediation, the connector never holds the credential bytes — it
// references its own manifest's `[capabilities.credential].kind`
// (e.g. "oauth2", "api_key") and the runtime resolves the bound vault
// entry, fails closed when none is bound, and injects the credential
// into the outbound request. Connectors that do not need a credential
// omit the field entirely.
type httpRequestEnvelope struct {
	Method     string            `json:"method"`
	URL        string            `json:"url"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       string            `json:"body,omitempty"`
	Credential string            `json:"credential,omitempty"`
}

// hostHTTPRequest implements `aileron_host.http_request(req_ptr, req_len) -> i32`.
//
// Returns 0 on success (response stored in state for subsequent
// `http_response_*` calls); -1 when the call is denied at the network
// boundary or fails; -2 when the JSON request envelope is malformed.
//
// On denial, the structured *Error is stuck on the state's `respErr`
// so [Connector.Invoke] can surface it instead of the generic
// connector_runtime_error path.
func hostHTTPRequest(ctx context.Context, mod api.Module, reqPtr, reqLen uint32) int32 {
	s := stateFromCtx(ctx)
	if s == nil {
		return -1
	}
	raw := readMemory(mod, reqPtr, reqLen)
	if raw == nil {
		s.mu.Lock()
		s.respErr = newConnectorRuntimeError("http_request: invalid memory range")
		s.mu.Unlock()
		return -2
	}
	var env httpRequestEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		s.mu.Lock()
		s.respErr = newConnectorRuntimeError(fmt.Sprintf("http_request: invalid JSON envelope: %s", err.Error()))
		s.mu.Unlock()
		return -2
	}
	if env.URL == "" {
		s.mu.Lock()
		s.respErr = newConnectorRuntimeError("http_request: empty URL")
		s.mu.Unlock()
		return -2
	}

	// Capability gate: ADR-0005 acceptance #3.
	if err := s.policy.CheckURL(env.URL); err != nil {
		s.mu.Lock()
		s.respErr = err.(*Error)
		s.mu.Unlock()
		return -1
	}
	// Action-boundary gate (ADR-0003 §"defense in depth", issue AC #7).
	// v1's action manifest declares network capability as the URL host:port
	// (matching the connector manifest's grant). When AllowedAuthority is
	// non-empty, every URL the connector dials must fall within that subset.
	if len(s.actionGrant) > 0 {
		host := hostFromURL(env.URL)
		if _, ok := s.actionGrant[host]; !ok {
			granted := make([]string, 0, len(s.actionGrant))
			for h := range s.actionGrant {
				granted = append(granted, h)
			}
			s.mu.Lock()
			s.respErr = &Error{
				Class:    ClassCapabilityDenied,
				Boundary: BoundarySandbox,
				Message:  "host:port not in action's declared capability subset",
				Details: map[string]any{
					"requested":       "network:" + host,
					"action_subset":   prefixed(granted, "network:"),
					"boundary_detail": "action",
				},
			}
			s.mu.Unlock()
			return -1
		}
	}

	method := env.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if env.Body != "" {
		body = bytes.NewReader([]byte(env.Body))
	}
	req, err := http.NewRequestWithContext(ctx, method, env.URL, body)
	if err != nil {
		s.mu.Lock()
		s.respErr = newConnectorRuntimeError(fmt.Sprintf("http_request: build: %s", err.Error()))
		s.mu.Unlock()
		return -1
	}
	for k, v := range env.Headers {
		req.Header.Set(k, v)
	}
	// Credential mediation (ADR-0005). When the connector asks for a
	// credential to be attached, the host resolves the bound vault
	// entry and injects it; the connector never holds the bytes. The
	// envelope's `credential` field references the connector's own
	// manifest kind — a connector cannot forge a different connector's
	// kind because each runs in its own sandbox and the validation is
	// against its own manifest.
	if env.Credential != "" {
		if denyErr := injectCredential(ctx, s, req, env.Credential); denyErr != nil {
			s.mu.Lock()
			s.respErr = denyErr
			s.mu.Unlock()
			return -1
		}
	}
	resp, err := s.doer.Do(req)
	if err != nil {
		s.mu.Lock()
		s.respErr = newConnectorRuntimeError(fmt.Sprintf("http_request: %s", err.Error()))
		s.mu.Unlock()
		return -1
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		s.mu.Lock()
		s.respErr = newConnectorRuntimeError(fmt.Sprintf("http_request: read body: %s", err.Error()))
		s.mu.Unlock()
		return -1
	}
	s.mu.Lock()
	s.response = respBody
	s.respCode = resp.StatusCode
	s.respErr = nil
	s.mu.Unlock()
	return 0
}

// hostHTTPResponseSize implements `aileron_host.http_response_size() -> i32`.
// Returns the most recent response's body length in bytes, or -1 when
// no response is buffered.
func hostHTTPResponseSize(ctx context.Context) int32 {
	s := stateFromCtx(ctx)
	if s == nil {
		return -1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.response == nil {
		return -1
	}
	return int32(len(s.response))
}

// hostHTTPResponseStatus implements `aileron_host.http_response_status() -> i32`.
// Returns the most recent response's HTTP status code; -1 when no
// response is buffered.
func hostHTTPResponseStatus(ctx context.Context) int32 {
	s := stateFromCtx(ctx)
	if s == nil {
		return -1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.respCode == 0 {
		return -1
	}
	return int32(s.respCode)
}

// hostHTTPResponseRead implements `aileron_host.http_response_read(dst_ptr, dst_len) -> i32`.
// Copies up to `dstLen` bytes of the most recent response body into
// memory at `dstPtr`. Returns the number of bytes written; -1 when no
// response is buffered or the destination range is invalid.
func hostHTTPResponseRead(ctx context.Context, mod api.Module, dstPtr, dstLen uint32) int32 {
	s := stateFromCtx(ctx)
	if s == nil {
		return -1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.response == nil {
		return -1
	}
	n := uint32(len(s.response))
	if n > dstLen {
		n = dstLen
	}
	if n == 0 {
		return 0
	}
	wrote := writeMemory(mod, dstPtr, s.response[:n])
	if wrote == 0 {
		return -1
	}
	return int32(wrote)
}

// injectCredential resolves the bound credential and attaches it to
// `req` per the kind's wire convention. Returns a structured *Error on
// any of the failure modes:
//
//   - capability_denied: connector requested a kind that does not
//     match its manifest's `[capabilities.credential].kind`, or its
//     manifest declares no credential capability at all.
//   - binding_required: the action did not declare a binding for this
//     connector (no resolver wired), or the vault entry is missing.
//   - capability_denied (kind mismatch): the bound vault entry's
//     metadata kind does not match what the connector's manifest
//     declares.
//
// The credential bytes never leave this function; the only place they
// land is `req.Header` for the actual outbound dial.
func injectCredential(ctx context.Context, s *hostState, req *http.Request, requested string) *Error {
	if s.expectedCredentialKind == "" {
		return newCapabilityDenied(
			"http_request: connector did not declare [capabilities.credential]",
			map[string]any{
				"requested":       "credential:" + requested,
				"connector":       s.connectorFQN,
				"boundary_detail": "connector_manifest",
			})
	}
	if requested != s.expectedCredentialKind {
		return newCapabilityDenied(
			"http_request: requested credential kind does not match connector manifest",
			map[string]any{
				"requested":       "credential:" + requested,
				"declared":        "credential:" + s.expectedCredentialKind,
				"connector":       s.connectorFQN,
				"boundary_detail": "connector_manifest",
			})
	}
	if s.credentialResolver == nil {
		return newBindingRequired(
			fmt.Sprintf(
				"http_request: no credential binding found for connector %s (kind: %s); run `aileron binding setup %s` to create one or `aileron binding list` to inspect existing bindings",
				s.connectorFQN, s.expectedCredentialKind, s.connectorFQN),
			map[string]any{
				"connector":       s.connectorFQN,
				"capability_kind": s.expectedCredentialKind,
				"boundary_detail": "action",
			})
	}
	cred, resolveErr := s.credentialResolver.Resolve(ctx)
	if resolveErr != nil {
		switch {
		case errors.Is(resolveErr, credential.ErrBindingMissing),
			errors.Is(resolveErr, credential.ErrNoBindingResolver):
			return newBindingRequired(
				resolveErr.Error(),
				map[string]any{
					"connector":       s.connectorFQN,
					"capability_kind": s.expectedCredentialKind,
					"boundary_detail": "action",
				})
		case errors.Is(resolveErr, credential.ErrCredentialKindMismatch):
			return newCapabilityDenied(
				resolveErr.Error(),
				map[string]any{
					"connector":       s.connectorFQN,
					"capability_kind": s.expectedCredentialKind,
					"boundary_detail": "vault",
				})
		default:
			return newConnectorRuntimeError(fmt.Sprintf(
				"http_request: resolve credential: %s", resolveErr.Error()))
		}
	}
	switch cred.Kind {
	case "oauth2":
		// RFC 6750 fixes OAuth2 access tokens as
		// `Authorization: Bearer <token>`. The manifest's `header` and
		// `format` fields are rejected at validation for oauth2 kind,
		// so we never have to consult them here.
		req.Header.Set("Authorization", "Bearer "+string(cred.Value))
	case "api_key":
		header, format := apiKeyInjection(s.credentialHeader, s.credentialFormat)
		req.Header.Set(header, strings.ReplaceAll(format, "{key}", string(cred.Value)))
	default:
		return newCapabilityDenied(
			"http_request: unsupported credential kind for v1 injection",
			map[string]any{
				"connector":       s.connectorFQN,
				"capability_kind": cred.Kind,
				"boundary_detail": "runtime",
			})
	}
	return nil
}

// apiKeyInjection resolves the `(header, format)` pair the api_key
// injection path uses. Defaults match the pre-#917 wire shape so
// connectors that don't set either field keep their current behavior.
// The format is validated at manifest install time to contain the
// `{key}` placeholder, so this returns the raw string for the caller
// to substitute.
func apiKeyInjection(header, format string) (string, string) {
	if header == "" {
		header = "Authorization"
	}
	if format == "" {
		format = "Bearer {key}"
	}
	return header, format
}

// hostFromURL extracts the host:port portion of a URL using the same
// scheme-default rules as HostPolicy.CheckURL. Used to compare against
// the action-boundary capability subset. Returns the raw URL on parse
// failure (or when the parsed URL has no host) so downstream
// comparisons fail closed.
func hostFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u == nil || u.Hostname() == "" {
		return rawURL
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return host + ":" + port
}
