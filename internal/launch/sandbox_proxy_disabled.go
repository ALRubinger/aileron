package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// stderrForProxyDisabledReport is the writer reportSandboxProxyDisabled
// uses for best-effort warning output when the daemon call fails. It's
// a package-level var so tests can substitute io.Discard (or a buffer)
// without racing on os.Stderr.
var stderrForProxyDisabledReport io.Writer = os.Stderr

// sandboxProxyContractDocsURL points the user at the BYO image proxy
// contract page (U2 / docs/development/sandbox-agent-images.md). Used
// in the actionable error surfaced when proxy bootstrap is requested
// but the resolved sandbox image lacks the required helpers.
const sandboxProxyContractDocsURL = "https://docs.withaileron.ai/development/sandbox-agent-images/#byo-image-proxy-contract"

// recordSandboxProxyDisabled POSTs to the daemon's
// /v1/sandbox-proxy/disabled endpoint to record that proxy bootstrap
// is not active for this launch. The body shape matches the OpenAPI
// SandboxProxyDisabledRequest schema. Returns the daemon's error so
// callers can decide whether to surface it (production currently
// treats it as best-effort and only logs failures).
func (c *daemonClient) RecordSandboxProxyDisabled(ctx context.Context, sessionID, reason, sandboxMode, sandboxImage string) error {
	if c == nil {
		return fmt.Errorf("daemon client is nil")
	}
	body := map[string]any{
		"session_id": sessionID,
		"reason":     reason,
	}
	if sandboxMode != "" {
		body["sandbox_mode"] = sandboxMode
	}
	if sandboxImage != "" {
		body["sandbox_image"] = sandboxImage
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal sandbox-proxy-disabled request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/sandbox-proxy/disabled", bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("post /v1/sandbox-proxy/disabled: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return httpStatusError(resp, "POST /v1/sandbox-proxy/disabled")
	}
	return nil
}

// reportSandboxProxyDisabled is the launcher-side wrapper around the
// daemon HTTP call. It tolerates a missing daemon, a missing session
// id (for the refuse-before-session-register path), and any transient
// HTTP failure — none of those should block the launcher from
// proceeding (or, in the refuse case, from exiting with the original
// error). Failures are sent to stderr so a developer-mode launch can
// still see them; we don't have a logger at this layer.
func reportSandboxProxyDisabled(ctx context.Context, client *daemonClient, sessionID, reason, sandboxMode, sandboxImage string) {
	if client == nil {
		return
	}
	if strings.TrimSpace(reason) == "" {
		return
	}
	postCtx, cancel := context.WithTimeout(ctx, daemonHTTPTimeout)
	defer cancel()
	if err := client.RecordSandboxProxyDisabled(postCtx, sessionID, reason, sandboxMode, sandboxImage); err != nil {
		fmt.Fprintf(stderrForProxyDisabledReport, "aileron: warning: record sandbox.proxy.disabled (%s): %v\n", reason, err)
	}
}

// sandboxProxyRefuseError renders the user-visible error when proxy
// bootstrap is explicitly requested against a sandbox mode that can't
// support it (e.g. --sandbox-proxy=on with --sandbox=off). The error
// names the requested mode and points at the opt-out flag.
func sandboxProxyRefuseError(reason, sandboxMode string) error {
	switch reason {
	case sandboxProxyReasonUnsupportedSandboxMode:
		mode := sandboxMode
		if strings.TrimSpace(mode) == "" {
			mode = "off"
		}
		return fmt.Errorf(
			"sandbox proxy bootstrap requested but --sandbox=%s does not support it; rerun with --sandbox=docker or --sandbox=podman, or pass --sandbox-proxy=off to disable bootstrap",
			mode,
		)
	default:
		return fmt.Errorf("sandbox proxy bootstrap refused: %s", reason)
	}
}

// sandboxProxyPreflightFailedError wraps a sandbox validation failure
// with an actionable hint pointing at the BYO image proxy contract docs
// page and the opt-out flag. Called when proxy bootstrap is active and
// the validate step fails — most commonly because the BYO image lacks
// aileron-install-proxy-ca / aileron-run-with-proxy-ca.
func sandboxProxyPreflightFailedError(image string, cause error) error {
	return fmt.Errorf(
		"sandbox proxy bootstrap preflight failed for image %s: %w; see %s for the BYO image proxy contract, or rerun with --sandbox-proxy=off to disable bootstrap",
		image, cause, sandboxProxyContractDocsURL,
	)
}

// isSandboxProxyContractFailure reports whether the given validate
// error came from the proxy-helper preflight (the bash branch that
// fails when aileron-install-proxy-ca or aileron-run-with-proxy-ca is
// missing, or when --check fails). We can't introspect exit codes
// reliably so we match on the contract-specific error strings the
// runtime contract probe emits.
func isSandboxProxyContractFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range []string{
		"aileron-install-proxy-ca",
		"aileron-run-with-proxy-ca",
		"sandbox proxy bootstrap requires",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
