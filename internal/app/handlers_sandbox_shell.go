package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/model"
)

// sandboxShellDenyPatternEnv is the daemon-scoped env var the slice-6 deny
// trigger reads at startup. When set to a non-empty value, the daemon
// compiles it as a regex and refuses to start if compilation fails (KTD2).
// When unset, the allow path is byte-for-byte unchanged from slice 5 (R6).
const sandboxShellDenyPatternEnv = "AILERON_SANDBOX_SHELL_DENY_PATTERN"

const (
	sandboxShellDecisionStatusDecided      = "decided"
	sandboxShellDecisionReasonAllowOnly    = "sandbox shell decision contract is allow-only in this implementation slice"
	sandboxShellDecisionReasonAllowNoMatch = "command does not match deny pattern"
	sandboxShellDenyReasonPrefix           = "matched deny pattern: "
)

// loadSandboxShellDenyPattern reads AILERON_SANDBOX_SHELL_DENY_PATTERN and
// returns the compiled regex (or nil if unset). A set-but-invalid pattern
// returns a non-nil error so app.New can refuse to start (R7 fail-closed +
// KTD2 startup refusal).
func loadSandboxShellDenyPattern() (*regexp.Regexp, error) {
	raw := os.Getenv(sandboxShellDenyPatternEnv)
	if raw == "" {
		return nil, nil
	}
	re, err := regexp.Compile(raw)
	if err != nil {
		return nil, fmt.Errorf("%s=%q: %w", sandboxShellDenyPatternEnv, raw, err)
	}
	return re, nil
}

// DecideSandboxShellCommand records the daemon-side shell mediation boundary.
// Returns `allow` by default; returns `deny` when the command matches the
// daemon's compiled AILERON_SANDBOX_SHELL_DENY_PATTERN regex (KTD2). The deny
// reason format is `matched deny pattern: <pattern source>`; the matched
// pattern source is also surfaced as a separate field on the response and on
// the audit payload as `aileron.shell.matched_pattern` (KTD4).
func (s *apiServer) DecideSandboxShellCommand(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req api.SandboxShellDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid JSON request body")
		return
	}

	command := strings.TrimSpace(req.Command)
	if command == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "command field is required")
		return
	}

	decision, reason, matchedPattern := s.sandboxShellEvaluate(command)
	latencyMs := time.Since(start).Milliseconds()
	auditID := s.recordSandboxShellDecision(r, req, command, string(decision), reason, matchedPattern, latencyMs)

	resp := api.SandboxShellDecisionResponse{
		Status:   sandboxShellDecisionStatusDecided,
		Decision: decision,
		AuditId:  auditID,
		Reason:   ptrNonEmpty(reason),
	}
	if matchedPattern != "" {
		resp.MatchedPattern = ptrNonEmpty(matchedPattern)
	}
	writeJSON(w, http.StatusOK, resp)
}

// sandboxShellEvaluate is the deny-pattern evaluation seam used by the
// handler. When no deny pattern is configured, the allow path returns the
// slice-5 reason verbatim (R6). When configured, a match returns `deny` with
// the KTD2 stable reason format and the pattern source; a miss returns `allow`
// with a distinct "no match" reason so audits stay informative.
func (s *apiServer) sandboxShellEvaluate(command string) (decision api.SandboxShellDecisionResponseDecision, reason string, matchedPattern string) {
	if s.sandboxShellDenyPattern == nil {
		return api.SandboxShellDecisionResponseDecisionAllow, sandboxShellDecisionReasonAllowOnly, ""
	}
	src := s.sandboxShellDenyPattern.String()
	if s.sandboxShellDenyPattern.MatchString(command) {
		return api.SandboxShellDecisionResponseDecisionDeny, sandboxShellDenyReasonPrefix + src, src
	}
	return api.SandboxShellDecisionResponseDecisionAllow, sandboxShellDecisionReasonAllowNoMatch, ""
}

func (s *apiServer) recordSandboxShellDecision(r *http.Request, req api.SandboxShellDecisionRequest, command, decision, reason, matchedPattern string, latencyMs int64) string {
	if s.auditRecorder == nil {
		if s.newID != nil {
			return s.newID()
		}
		return audit.DefaultIDFn()
	}
	payload := map[string]any{
		"aileron.shell.boundary":   "sandbox_shell",
		"aileron.shell.command":    command,
		"aileron.shell.decision":   decision,
		"aileron.shell.reason":     reason,
		"aileron.shell.latency_ms": latencyMs,
	}
	if matchedPattern != "" {
		payload["aileron.shell.matched_pattern"] = matchedPattern
	}
	if cwd := strings.TrimSpace(valueOrEmpty(req.Cwd)); cwd != "" {
		payload["aileron.shell.cwd"] = cwd
	}
	if shell := strings.TrimSpace(valueOrEmpty(req.Shell)); shell != "" {
		payload["aileron.shell.path"] = shell
	}
	if req.Pid != nil {
		payload["aileron.shell.pid"] = *req.Pid
	}
	if req.Ppid != nil {
		payload["aileron.shell.ppid"] = *req.Ppid
	}
	if sessionID := strings.TrimSpace(r.Header.Get("X-Aileron-Session-Id")); sessionID != "" {
		payload["aileron.session.id"] = sessionID
	}
	return s.auditRecorder.RecordSuccess(
		r.Context(),
		model.EventTypeSandboxShellDecided,
		model.ActorRef{Type: model.ActorTypeAgent, ID: "sandbox-shell"},
		payload,
	)
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
