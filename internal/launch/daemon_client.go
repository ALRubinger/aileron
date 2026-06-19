package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/ALRubinger/aileron/internal/vault"
)

// daemonHTTPTimeout caps the launch process's HTTP calls to the
// daemon. Session-create / session-end / vault-status are all small
// JSON round-trips on loopback; 5 seconds is conservative.
const daemonHTTPTimeout = 5 * time.Second

// defaultAgentCredentialPurpose is the third vault-path segment used
// when no explicit purpose is supplied. It matches the daemon-side
// default (`agents/<name>/oauth`), so a request carrying this purpose
// is omitted from the query string and routes byte-for-byte like the
// pre-purpose wire shape.
const defaultAgentCredentialPurpose = "oauth"

// daemonClient is a thin HTTP client over the daemon's /v1 surface,
// used by Launch to register and end the agent's session and to peek
// at the vault state for the startup banner. Production code creates
// one per Launch invocation.
type daemonClient struct {
	baseURL string
	token   string
	client  *http.Client
}

// newDaemonClient wraps the daemon's URL (e.g. "http://127.0.0.1:54321")
// in a thin HTTP client. baseURL has any trailing slash trimmed and
// is treated as the root the /v1 paths attach to.
func newDaemonClient(baseURL string, token ...string) *daemonClient {
	var tok string
	if len(token) > 0 {
		tok = token[0]
	}
	return &daemonClient{
		baseURL: trimTrailingSlash(baseURL),
		token:   tok,
		client:  &http.Client{Timeout: daemonHTTPTimeout},
	}
}

// RegisterSession POSTs /v1/sessions with the agent name and working
// directory. The daemon mints a ULID, stamps StartedAt, and returns
// the session record; we hand the ID back to the caller.
func (c *daemonClient) RegisterSession(ctx context.Context, agent, workingDir string) (string, error) {
	body, err := json.Marshal(map[string]string{
		"agent":       agent,
		"working_dir": workingDir,
	})
	if err != nil {
		return "", fmt.Errorf("marshal create-session request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/sessions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("post /v1/sessions: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", httpStatusError(resp, "POST /v1/sessions")
	}
	var sess struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		return "", fmt.Errorf("decode create-session response: %w", err)
	}
	if sess.ID == "" {
		return "", fmt.Errorf("daemon returned empty session id")
	}
	return sess.ID, nil
}

// EndSession POSTs /v1/sessions/{id}/end with the agent's exit code.
// The daemon stamps EndedAt; passing exitCode == nil records the
// orphaned-shape (matches the JSONL reaper's behavior on daemon
// restart) — used when launch can't observe a clean exit.
func (c *daemonClient) EndSession(ctx context.Context, sessionID string, exitCode *int) error {
	body, err := json.Marshal(map[string]any{"exit_code": exitCode})
	if err != nil {
		return fmt.Errorf("marshal end-session request: %w", err)
	}
	endpoint := c.baseURL + "/v1/sessions/" + url.PathEscape(sessionID) + "/end"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("post end-session: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpStatusError(resp, "POST /v1/sessions/{id}/end")
	}
	return nil
}

// LocalVaultLocked GETs /v1/vault/local/status to learn whether the
// daemon's local vault is currently locked. Returns (locked, ok)
// where ok is false when the daemon doesn't expose the endpoint
// (cloud-shaped deployments) or doesn't respond — caller treats
// !ok as "unknown" and skips the banner hint.
func (c *daemonClient) LocalVaultLocked(ctx context.Context) (locked bool, ok bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/vault/local/status", nil)
	if err != nil {
		return false, false
	}
	c.authorize(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, false
	}
	var body struct {
		Locked bool `json:"locked"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, false
	}
	return body.Locked, true
}

// ErrAgentCredentialsNotFound is the typed Go error the daemon
// client surfaces when the daemon returns
// `{error: {code: "vault_not_found"}}`. The launcher discriminates
// this from a locked vault so the in-container-login bootstrap path
// fires only on a genuinely empty vault entry, never on a locked
// vault the user just needs to unlock. ADR-0025.
var ErrAgentCredentialsNotFound = errors.New("daemon: agent credentials not found")

// agentCredentialsBody mirrors the OpenAPI AgentCredentials schema.
// The launcher's daemon client decodes into this rather than the
// generated api.AgentCredentials shape so the launch package stays
// independent of `internal/api/gen` (the generated package brings
// in the full server-side type tree, which the launcher doesn't
// otherwise depend on).
type agentCredentialsBody struct {
	Value    []byte                       `json:"value"`
	Metadata *agentCredentialsMetadataDTO `json:"metadata,omitempty"`
}

type agentCredentialsMetadataDTO struct {
	Type        string            `json:"type,omitempty"`
	Environment string            `json:"environment,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

// agentCredentialsEndpoint builds the per-agent credential URL for the
// given agent name and purpose. The `purpose` query parameter is
// omitted when it is the default (`oauth`) so a default request is
// byte-for-byte identical on the wire to the pre-purpose behavior; a
// non-default purpose is appended as `?purpose=<value>`.
func (c *daemonClient) agentCredentialsEndpoint(name, purpose string) string {
	endpoint := c.baseURL + "/v1/vault/agents/" + url.PathEscape(name) + "/credentials"
	if purpose != "" && purpose != defaultAgentCredentialPurpose {
		endpoint += "?" + url.Values{"purpose": {purpose}}.Encode()
	}
	return endpoint
}

// GetAgentCredentials fetches the per-agent credential envelope at
// `agents/<name>/<purpose>` through the daemon's HTTP surface
// (purpose defaults to `oauth`). The
// daemon's `vault_not_found` error code surfaces as
// [ErrAgentCredentialsNotFound]; a locked vault surfaces as
// [vault.ErrCredentialUnavailable]. Callers wrap typed checks with
// `errors.Is`.
func (c *daemonClient) GetAgentCredentials(ctx context.Context, name, purpose string) (vault.Secret, error) {
	endpoint := c.agentCredentialsEndpoint(name, purpose)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return vault.Secret{}, err
	}
	c.authorize(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return vault.Secret{}, fmt.Errorf("get agent credentials: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var body agentCredentialsBody
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return vault.Secret{}, fmt.Errorf("decode agent credentials: %w", err)
		}
		secret := vault.Secret{Value: body.Value}
		if body.Metadata != nil {
			secret.Metadata = vault.Metadata{
				Type:        body.Metadata.Type,
				Environment: body.Metadata.Environment,
				Labels:      body.Metadata.Labels,
			}
		}
		return secret, nil
	case http.StatusNotFound:
		// Discriminate by body code, not status alone, so a future
		// 404 envelope shape change doesn't silently re-route the
		// in-container-login fallthrough.
		if classifyVaultError(resp) == "vault_not_found" {
			return vault.Secret{}, ErrAgentCredentialsNotFound
		}
		return vault.Secret{}, httpStatusError(resp, "GET agent credentials")
	case http.StatusLocked:
		if classifyVaultError(resp) == "vault_locked" {
			return vault.Secret{}, vault.ErrCredentialUnavailable
		}
		return vault.Secret{}, httpStatusError(resp, "GET agent credentials")
	default:
		return vault.Secret{}, httpStatusError(resp, "GET agent credentials")
	}
}

// ErrUserCredentialsNotFound is the typed Go error the daemon client
// surfaces when a GET against the user-level credential endpoint
// returns `{error: {code: "vault_not_found"}}`. The GitHub injector
// discriminates this from a locked vault so a genuinely empty
// `user/<service>` entry yields a clean, unauthenticated launch
// rather than aborting. Mirrors [ErrAgentCredentialsNotFound] for the
// agent-independent user namespace (#1149).
var ErrUserCredentialsNotFound = errors.New("daemon: user credentials not found")

// GetUserCredentials fetches the user-level credential envelope at
// `user/<service>` through the daemon's HTTP surface. This namespace
// is agent-independent: the credential belongs to the user, not a
// single agent (e.g. `user/github`). The daemon's `vault_not_found`
// error code surfaces as [ErrUserCredentialsNotFound]; a locked vault
// surfaces as [vault.ErrCredentialUnavailable]. Callers wrap typed
// checks with `errors.Is`.
//
// Structurally identical to [GetAgentCredentials] — same envelope
// decode, same body-code discrimination — but targets the
// `/v1/vault/user/{service}/credentials` route owned by the merged
// OpenAPI spec.
func (c *daemonClient) GetUserCredentials(ctx context.Context, service string) (vault.Secret, error) {
	endpoint := c.baseURL + "/v1/vault/user/" + url.PathEscape(service) + "/credentials"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return vault.Secret{}, err
	}
	c.authorize(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return vault.Secret{}, fmt.Errorf("get user credentials: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var body agentCredentialsBody
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return vault.Secret{}, fmt.Errorf("decode user credentials: %w", err)
		}
		secret := vault.Secret{Value: body.Value}
		if body.Metadata != nil {
			secret.Metadata = vault.Metadata{
				Type:        body.Metadata.Type,
				Environment: body.Metadata.Environment,
				Labels:      body.Metadata.Labels,
			}
		}
		return secret, nil
	case http.StatusNotFound:
		// Any 404 maps to ErrUserCredentialsNotFound, regardless of body
		// code. Unlike the per-agent path — where a bare 404 could mask a
		// daemon routing bug worth surfacing — user-level GitHub injection
		// is purely additive and optional: a missing route (older daemon),
		// an empty entry (`vault_not_found`), or any generic 404 all mean
		// "no usable token this launch," which the injector turns into a
		// clean, unauthenticated launch rather than an abort. Mapping every
		// 404 here keeps the optional feature from ever breaking a launch.
		return vault.Secret{}, ErrUserCredentialsNotFound
	case http.StatusLocked:
		if classifyVaultError(resp) == "vault_locked" {
			return vault.Secret{}, vault.ErrCredentialUnavailable
		}
		return vault.Secret{}, httpStatusError(resp, "GET user credentials")
	default:
		return vault.Secret{}, httpStatusError(resp, "GET user credentials")
	}
}

// PutAgentCredentials writes the per-agent credential envelope back
// to the daemon's vault. Used by the launcher's Capture pass on
// clean container exit and by Codex's pre-launch refresh hook (the
// rotated bundle must be in vault before container start per AE6).
func (c *daemonClient) PutAgentCredentials(ctx context.Context, name, purpose string, secret vault.Secret) error {
	body := agentCredentialsBody{Value: secret.Value}
	if secret.Metadata.Type != "" || secret.Metadata.Environment != "" || len(secret.Metadata.Labels) > 0 {
		body.Metadata = &agentCredentialsMetadataDTO{
			Type:        secret.Metadata.Type,
			Environment: secret.Metadata.Environment,
			Labels:      secret.Metadata.Labels,
		}
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal agent credentials: %w", err)
	}
	endpoint := c.agentCredentialsEndpoint(name, purpose)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("put agent credentials: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusLocked:
		if classifyVaultError(resp) == "vault_locked" {
			return vault.ErrCredentialUnavailable
		}
		return httpStatusError(resp, "PUT agent credentials")
	default:
		return httpStatusError(resp, "PUT agent credentials")
	}
}

// classifyVaultError peeks at the response body for the daemon's
// named error code (`vault_not_found`, `vault_locked`). Returns the
// empty string when the body can't be decoded. Consumes the body —
// callers that fall through to `httpStatusError` must hand it a
// fresh response; in this file the switch arms call exactly one of
// the two and return immediately.
func classifyVaultError(resp *http.Response) string {
	// Buffer the body so a future `httpStatusError` re-decode still
	// sees content. Bodies on the daemon's error path are small JSON
	// envelopes; reading them whole is cheap.
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return ""
	}
	return env.Error.Code
}

func (c *daemonClient) authorize(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// httpStatusError reads the response body and returns a human-friendly
// error mentioning the operation, the status, and (if the body decoded
// as JSON) the daemon's `error` field.
func httpStatusError(resp *http.Response, op string) error {
	b, _ := io.ReadAll(resp.Body)
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(b, &e)
	if e.Error.Code != "" {
		return fmt.Errorf("%s: status %d: %s — %s", op, resp.StatusCode, e.Error.Code, e.Error.Message)
	}
	return fmt.Errorf("%s: status %d: %s", op, resp.StatusCode, string(b))
}

// trimTrailingSlash strips a single trailing "/" so the caller can
// concatenate paths without producing a doubled slash.
func trimTrailingSlash(u string) string {
	for len(u) > 1 && u[len(u)-1] == '/' {
		u = u[:len(u)-1]
	}
	return u
}
