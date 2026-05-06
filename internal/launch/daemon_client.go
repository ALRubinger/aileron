package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// daemonHTTPTimeout caps the launch process's HTTP calls to the
// daemon. Session-create / session-end / vault-status are all small
// JSON round-trips on loopback; 5 seconds is conservative.
const daemonHTTPTimeout = 5 * time.Second

// daemonClient is a thin HTTP client over the daemon's /v1 surface,
// used by Launch to register and end the agent's session and to peek
// at the vault state for the startup banner. Production code creates
// one per Launch invocation.
type daemonClient struct {
	baseURL string
	client  *http.Client
}

// newDaemonClient wraps the daemon's URL (e.g. "http://127.0.0.1:54321")
// in a thin HTTP client. baseURL has any trailing slash trimmed and
// is treated as the root the /v1 paths attach to.
func newDaemonClient(baseURL string) *daemonClient {
	return &daemonClient{
		baseURL: trimTrailingSlash(baseURL),
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

