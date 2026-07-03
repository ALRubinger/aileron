package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
)

// inContainerToolStepRunner is the production runtime.ToolStepRunner (#1829):
// it executes a `kind: tool` step's argv as a deterministic subprocess INSIDE
// the booted plan container. No sibling container is ever dispatched — the
// environment identity is the plan's single composed pin, entered once at the
// whole-plan boot.
//
// Safety posture:
//
//   - the runner refuses to exec outside the pinned image: the
//     AILERON_SKILL_IMAGE_BOOTED sentinel (injected by the image runner) must
//     be present, so a signed argv can never exec on the operator's host;
//   - a step with a SEALED reach (spec.Hosts non-empty) runs under a
//     daemon-minted step-scoped proxy credential registered for exactly those
//     hosts: the subprocess's HTTPS_PROXY is the boot proxy URL with the
//     scope token as the CONNECT password (the boot session id stays the
//     username, preserving CA continuity with the CA bundle the boot already
//     trusts). Obtaining the scope FAILS CLOSED: a sealed step that cannot
//     mint a scope errors and never runs unscoped;
//   - the scoped subprocess env removes AILERON_TOKEN so the step cannot
//     trivially reconstruct the unscoped boot credential;
//   - the argv is exec'd directly (no shell), input is written to
//     MountPath/input.json, and the collected output is read back with the
//     Lstat regular-file guard.
type inContainerToolStepRunner struct{}

// toolStepInputFile is the filename the resolved step input is written to
// inside the mount dir. The tool reads its input from MountPath/<this>.
const toolStepInputFile = "input.json"

// toolStepExecCommand execs the step argv with the given env, streaming
// stdout/stderr. It is a package variable (mirroring containerRunFlightPlan)
// so CLI unit tests fake the subprocess and assert on the argv/env without
// running anything.
var toolStepExecCommand = func(ctx context.Context, env []string, stdout, stderr io.Writer, argv []string) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func (inContainerToolStepRunner) Run(ctx context.Context, spec runtime.ToolStepSpec) (runtime.ToolStepResult, error) {
	// The sentinel guard: tool steps run ONLY inside the pinned plan image
	// (runtime.Run refuses unpinned tool-step plans; this is the second,
	// process-local lock). Without it a misconfigured launch could exec the
	// signed argv on the operator's host.
	if os.Getenv(envSkillImageBooted) == "" {
		return runtime.ToolStepResult{}, fmt.Errorf("skill launch: tool step %q may only execute inside the pinned plan image (%s is not set); refusing to exec on the host", spec.StepID, envSkillImageBooted)
	}
	if len(spec.Command) == 0 {
		return runtime.ToolStepResult{}, fmt.Errorf("skill launch: tool step %q has no command", spec.StepID)
	}

	env := os.Environ()
	// A step with a sealed reach runs under a step-scoped proxy credential:
	// mint before exec, release after (best-effort; the daemon TTL is the
	// backstop). Fail closed: a sealed step never runs unscoped.
	if len(spec.Hosts) > 0 {
		scopedURL, release, err := mintToolStepScope(ctx, spec.StepID, spec.Hosts)
		if err != nil {
			return runtime.ToolStepResult{}, fmt.Errorf("skill launch: tool step %q has a sealed reach but no step scope could be obtained (refusing to run unscoped): %w", spec.StepID, err)
		}
		defer release()
		env = toolStepScopedEnv(env, scopedURL)
	}

	// Mount side: write the resolved binding input as JSON at
	// MountPath/input.json. The input is binding-resolved data only, never a
	// credential.
	if spec.MountPath != "" {
		if err := os.MkdirAll(spec.MountPath, 0o755); err != nil {
			return runtime.ToolStepResult{}, fmt.Errorf("skill launch: create tool mount dir: %w", err)
		}
		payload, err := json.Marshal(spec.Input)
		if err != nil {
			return runtime.ToolStepResult{}, fmt.Errorf("skill launch: serialize tool input: %w", err)
		}
		if err := os.WriteFile(filepath.Join(spec.MountPath, toolStepInputFile), payload, 0o644); err != nil {
			return runtime.ToolStepResult{}, fmt.Errorf("skill launch: write tool input: %w", err)
		}
	}

	// Exec the argv directly — no shell, no interpretation: the signed
	// invocation carries no injection surface.
	if err := toolStepExecCommand(ctx, env, os.Stdout, os.Stderr, spec.Command); err != nil {
		return runtime.ToolStepResult{}, fmt.Errorf("skill launch: tool step %q command failed: %w", spec.StepID, err)
	}

	// Collect side: read back the declared collect path. A step with no
	// collect produces no output.
	if spec.CollectPath == "" {
		return runtime.ToolStepResult{}, nil
	}
	// The tool controls the collect path's contents. A malicious tool could
	// write the collected file as a SYMLINK pointing at a sensitive path
	// readable by this process; a naive os.ReadFile would follow it and
	// smuggle those bytes into the step output (CWE-61). Lstat first and
	// refuse anything that is not a regular file.
	info, err := os.Lstat(spec.CollectPath)
	if err != nil {
		return runtime.ToolStepResult{}, fmt.Errorf("skill launch: collect output from %q: %w", spec.CollectPath, err)
	}
	if !info.Mode().IsRegular() {
		return runtime.ToolStepResult{}, fmt.Errorf("skill launch: collected output %q is not a regular file (refusing to follow symlinks or read special files)", spec.CollectPath)
	}
	out, err := os.ReadFile(spec.CollectPath)
	if err != nil {
		return runtime.ToolStepResult{}, fmt.Errorf("skill launch: collect output from %q: %w", spec.CollectPath, err)
	}
	return runtime.ToolStepResult{Output: string(out)}, nil
}

// stepScopeRequest / stepScopeResponse mirror the daemon's
// SandboxProxyStepScope wire shapes (POST /v1/sandbox-proxy/step-scopes).
// Kept local to the CLI so the launch path does not import the generated
// server types, matching auditIngestRequest's discipline.
type stepScopeRequest struct {
	SessionID string   `json:"session_id"`
	StepID    string   `json:"step_id"`
	Hosts     []string `json:"hosts"`
}

type stepScopeResponse struct {
	ScopeID string `json:"scope_id"`
	Token   string `json:"token"`
}

// mintToolStepScope mints a daemon step-scoped proxy credential for one tool
// step's sealed reach and returns the scoped proxy URL the subprocess egresses
// through plus a release func (DELETE, best-effort; the TTL is the backstop).
//
// The boot proxy session is recovered from the boot HTTPS_PROXY env (#1828
// injected `http://<sessionID>:<daemonToken>@host:port`): the scoped URL
// keeps the same base and session username — preserving CA continuity with
// the boot session CA the container already trusts — and swaps the password
// for the minted scope token. Daemon coordinates come from the #1759 boot
// env (AILERON_API_URL/AILERON_TOKEN via bindingAPIBaseURL +
// setDaemonAuthorization, the same helpers the dispatcher and audit sink
// use).
func mintToolStepScope(ctx context.Context, stepID string, hosts []string) (string, func(), error) {
	bootProxy := strings.TrimSpace(os.Getenv("HTTPS_PROXY"))
	if bootProxy == "" {
		bootProxy = strings.TrimSpace(os.Getenv("https_proxy"))
	}
	if bootProxy == "" {
		return "", nil, fmt.Errorf("no boot proxy environment (HTTPS_PROXY) is present; the sealed reach cannot be scoped")
	}
	parsed, err := url.Parse(bootProxy)
	if err != nil || parsed.Host == "" || parsed.User == nil || parsed.User.Username() == "" {
		return "", nil, fmt.Errorf("boot proxy URL carries no session identity; the sealed reach cannot be scoped")
	}
	sessionID := parsed.User.Username()

	base, err := bindingAPIBaseURL()
	if err != nil {
		return "", nil, fmt.Errorf("resolve daemon URL: %w", err)
	}
	body, err := json.Marshal(stepScopeRequest{SessionID: sessionID, StepID: stepID, Hosts: hosts})
	if err != nil {
		return "", nil, fmt.Errorf("encode step-scope request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/sandbox-proxy/step-scopes", bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	setDaemonAuthorization(req)
	resp, err := actionsHTTPClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("mint step scope: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return "", nil, fmt.Errorf("daemon returned %d for step-scope mint: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var scope stepScopeResponse
	if err := json.Unmarshal(raw, &scope); err != nil {
		return "", nil, fmt.Errorf("decode step-scope response: %w", err)
	}
	if scope.Token == "" {
		return "", nil, fmt.Errorf("daemon minted an empty step-scope token")
	}

	// The scoped proxy URL: same base + session username, scope token as the
	// password. CA continuity holds because the session (and its CA) is the
	// boot session the container already trusts.
	scoped := *parsed
	scoped.User = url.UserPassword(sessionID, scope.Token)

	release := func() {
		// Best-effort release; the daemon TTL reaps an unreleased scope.
		req, err := http.NewRequest(http.MethodDelete, base+"/sandbox-proxy/step-scopes/"+url.PathEscape(scope.ScopeID), nil)
		if err != nil {
			return
		}
		setDaemonAuthorization(req)
		if resp, err := actionsHTTPClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	return scoped.String(), release, nil
}

// toolStepScopedEnv builds the scoped subprocess environment (#1829 decision
// 7): the runtime process env with HTTPS_PROXY/https_proxy — and
// HTTP_PROXY/http_proxy when originally set — replaced by the step-scoped
// proxy URL, and AILERON_TOKEN removed so the step cannot trivially
// reconstruct the unscoped boot credential. Everything else (the CA-bundle
// vars, NO_PROXY, AILERON_API_URL) passes through unchanged.
func toolStepScopedEnv(env []string, scopedURL string) []string {
	out := make([]string, 0, len(env)+4)
	hadHTTPProxyUpper, hadHTTPProxyLower := false, false
	for _, kv := range env {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			out = append(out, kv)
			continue
		}
		switch key {
		case "HTTPS_PROXY", "https_proxy", "AILERON_TOKEN":
			continue
		case "HTTP_PROXY":
			hadHTTPProxyUpper = true
			continue
		case "http_proxy":
			hadHTTPProxyLower = true
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "HTTPS_PROXY="+scopedURL, "https_proxy="+scopedURL)
	if hadHTTPProxyUpper {
		out = append(out, "HTTP_PROXY="+scopedURL)
	}
	if hadHTTPProxyLower {
		out = append(out, "http_proxy="+scopedURL)
	}
	return out
}
