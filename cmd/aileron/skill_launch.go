package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
)

// The Launch SPIs are wired here from package-level seams so CLI tests
// exercise the orchestration with fakes and no live daemon, mirroring
// skill_freeze.go's newDigestResolver/newFeatureComposer discipline. Each seam
// returns the daemon-backed implementation in production.
var newLaunchDispatcher = func() runtime.ActionDispatcher { return daemonDispatcher{} }
var newLaunchApprover = func() runtime.Approver { return daemonApprover{} }
var newLaunchAuditSink = func() runtime.AuditSink { return stdoutAuditSink{} }

// newLaunchImageRunner returns the production image runner that boots the
// verified pinned rung-1/rung-2 image and runs the plan inside it (#1731). It
// is a package-level seam so CLI tests swap in a fake that records the exact
// image string and never touches Docker, mirroring the other launch seams.
var newLaunchImageRunner = func() runtime.ImageRunner { return containerImageRunner{} }

// launchSeamForTest is the LLM seam the launch wires into the runtime. It is
// nil by default, which is the v1 contract: a plan with an llm-seam step
// errors unless a provider is configured, so a default launch reaches no LLM.
// It is a package-level seam so tests can supply a deterministic seam; there
// is no production LLM seam provider wired in v1.
var launchSeamForTest runtime.LLMSeam

// inputFlag collects repeated --input name=value launch overrides.
type inputFlag struct {
	values map[string]any
}

func (f *inputFlag) String() string { return "" }

func (f *inputFlag) Set(s string) error {
	i := strings.IndexByte(s, '=')
	if i <= 0 {
		return fmt.Errorf("--input must be name=value, got %q", s)
	}
	if f.values == nil {
		f.values = map[string]any{}
	}
	f.values[s[:i]] = s[i+1:]
	return nil
}

// runSkillLaunch implements `aileron skill launch <name>`. It loads a frozen
// version from the store, verifies it, resolves declared inputs, walks the
// deterministic step graph through the daemon-backed action boundary, and
// materializes declared artifacts into --out-dir. This is the Flight-Plan
// Launch runtime (#1511); it is distinct from the top-level `aileron launch`
// agent-sandbox command (a different subsystem).
func runSkillLaunch(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("skill launch", flag.ContinueOnError)
	flags.SetOutput(stderr)
	version := flags.String("version", "", "Frozen version id to launch (defaults to the only/most recent version)")
	outDir := flags.String("out-dir", ".", "Directory file-target artifacts are written to")
	var inputs inputFlag
	flags.Var(&inputs, "input", "Launch input override as name=value; repeatable")
	positionals, err := parseInterspersedFlags(flags, args)
	if err != nil {
		return 1
	}
	if len(positionals) != 1 {
		fmt.Fprintln(stderr, skillUsage)
		return 1
	}
	name := positionals[0]

	s := store.New(skillStoreDir)
	id, err := resolveLaunchVersion(s, name, *version)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	res, err := runtime.Run(context.Background(), runtime.Options{
		Store:      s,
		Name:       name,
		Version:    id,
		Inputs:     runtime.LaunchArgs(inputs.values),
		Dispatcher: newLaunchDispatcher(),
		Approver:   newLaunchApprover(),
		Audit:      newLaunchAuditSink(),
		// Seam is nil in v1 production: the LLM seam is unwired, so a plan with
		// an llm-seam step errors unless a provider is supplied. Tests inject a
		// deterministic seam through launchSeamForTest.
		Seam: launchSeamForTest,
		// ImageRunner boots the verified pinned rung-1/rung-2 image and runs the
		// plan inside it. When the frozen unit pins no image, the runtime never
		// touches this seam and stays on the in-process path.
		ImageRunner: newLaunchImageRunner(),
		OutDir:      *outDir,
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Launched %q\n", name)
	fmt.Fprintf(stdout, "  Version:     %s\n", id)
	fmt.Fprintf(stdout, "  ContentHash: %s\n", res.ContentHash)
	if len(res.ResolvedInputs) > 0 {
		fmt.Fprintln(stdout, "  Resolved inputs:")
		for k, v := range res.ResolvedInputs {
			fmt.Fprintf(stdout, "    %s = %v\n", k, v)
		}
	}
	if len(res.Artifacts) > 0 {
		fmt.Fprintln(stdout, "  Artifacts:")
		for _, a := range res.Artifacts {
			if a.Written {
				fmt.Fprintf(stdout, "    %s -> %s  %s\n", a.Name, a.Path, a.Digest)
			} else {
				fmt.Fprintf(stdout, "    %s (retained)  %s\n", a.Name, a.Digest)
			}
		}
	}
	if len(res.AuditIDs) > 0 {
		fmt.Fprintf(stdout, "  Audit records: %d\n", len(res.AuditIDs))
	}
	return 0
}

// resolveLaunchVersion resolves the version id to launch: the explicit
// --version when given, otherwise the single frozen version (or the most
// recent when several exist, since FrozenVersions returns sorted ids).
func resolveLaunchVersion(s *store.Store, name, version string) (string, error) {
	if version != "" {
		return version, nil
	}
	ids, err := s.FrozenVersions(name)
	if err != nil {
		return "", fmt.Errorf("list frozen versions for %q: %w", name, err)
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("skill %q has no frozen versions; run `aileron skill freeze %s` first", name, name)
	}
	return ids[len(ids)-1], nil
}

// daemonDispatcher dispatches an action through the daemon's
// POST /v1/actions/{name}/run endpoint. The action ref (aileron:<c>.<a>) maps
// to the daemon action name; credentials are injected host-side at the
// boundary, so the dispatcher never sees them. A 202 (approval pending) is
// surfaced as an error directing the operator to approve, since v1 launch runs
// unattended and does not poll the approval lifecycle.
type daemonDispatcher struct{}

func (daemonDispatcher) Dispatch(ctx context.Context, ref string, args map[string]any) (runtime.DispatchResult, error) {
	base, err := bindingAPIBaseURL()
	if err != nil {
		return runtime.DispatchResult{}, err
	}
	name := daemonActionName(ref)
	body, _ := json.Marshal(actionRunRequest{Args: args})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/actions/"+url.PathEscape(name)+"/run", bytes.NewReader(body))
	if err != nil {
		return runtime.DispatchResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	setDaemonAuthorization(req)
	resp, err := actionsHTTPClient.Do(req)
	if err != nil {
		return runtime.DispatchResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		var out actionRunResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return runtime.DispatchResult{}, fmt.Errorf("decode action result: %w", err)
		}
		return runtime.DispatchResult{Output: parseResultPayload(out.Result)}, nil
	case http.StatusAccepted:
		return runtime.DispatchResult{}, fmt.Errorf("action %q requires approval; approve it (aileron approval list) and re-launch", ref)
	default:
		return runtime.DispatchResult{}, fmt.Errorf("daemon returned %d for %q: %s", resp.StatusCode, ref, strings.TrimSpace(string(raw)))
	}
}

// daemonActionName maps a manifest action ref (aileron:<connector>.<action>)
// to the daemon action name. v1 uses the bare action segment as the daemon
// name; the connector binding is resolved daemon-side.
func daemonActionName(ref string) string {
	r := strings.TrimPrefix(ref, "aileron:")
	if i := strings.LastIndex(r, "."); i >= 0 {
		return r[i+1:]
	}
	return r
}

// parseResultPayload decodes the daemon's string result into a JSON map so the
// runtime can bind downstream steps against it. A non-JSON or empty result
// surfaces under a "result" key so a downstream binding still resolves.
func parseResultPayload(result *string) map[string]any {
	if result == nil || *result == "" {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(*result), &m); err == nil {
		return m
	}
	return map[string]any{"result": *result}
}

// daemonApprover defers to the daemon's own approval gate at the action
// boundary: the daemon's POST /run returns 202 when an action needs approval,
// which the dispatcher surfaces. The runtime-level approver therefore approves
// here so the routing is decided once, at the daemon boundary, rather than
// gated twice. A future interactive launch can replace this seam with one that
// drives the approval lifecycle in-process.
type daemonApprover struct{}

func (daemonApprover) Approve(_ context.Context, _ runtime.ApprovalRequest) (runtime.Decision, error) {
	return runtime.Decision{Approved: true}, nil
}

// stdoutAuditSink is a minimal launch audit sink. The customer-owned audit
// store is the daemon's configured sink; this CLI-side sink records a local
// summary id so the launch surfaces an audit count. It is replaced by a
// daemon-backed recorder when the daemon exposes a launch-audit endpoint.
type stdoutAuditSink struct{}

func (stdoutAuditSink) Record(_ context.Context, rec runtime.AuditRecord) string {
	if rec.ActionRef != "" {
		return "launch-audit-" + rec.ActionRef
	}
	return "launch-audit-summary"
}
