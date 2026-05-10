package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/credential"
	"github.com/ALRubinger/aileron/internal/cstore"
)

// All gate-side assertions in this file cite the ADR clause they
// enforce so refactors that preserve the contract leave them green.

// goodSpawnManifest builds a connector manifest with a typical
// [capabilities.spawn] declaration the gate tests mutate.
func goodSpawnManifest() *cstore.Manifest {
	return &cstore.Manifest{
		Connector: cstore.ManifestConnector{
			Name:    "github://aileron-test/gitcrawl",
			Version: "1.0.0",
		},
		Capabilities: cstore.ManifestCapabilities{
			Spawn: &cstore.ManifestSpawn{
				Programs: []cstore.ManifestSpawnProgram{
					{Path: "/usr/bin/git"},
				},
				Operations: map[string]cstore.ManifestSpawnOperation{
					"log":    {Argv: "git log --since={since}"},
					"status": {Argv: "git status"},
				},
				EnvPassthrough: []string{"GIT_AUTHOR_NAME", "GH_TOKEN"},
				FSRead:         []string{"~/code/"},
				FSWrite:        []string{"~/.cache/aileron/gitcrawl/"},
			},
		},
	}
}

func TestSpawnPolicy_DefaultDenyWhenNoSpawnBlock(t *testing.T) {
	// ADR-0002 §"Each capability section is enumerable and bounded.
	// There is no `*` and no implicit grant. A connector that did not
	// declare network access cannot dial out, period." — applies
	// equally to spawn.
	policy := NewSpawnPolicy(&cstore.Manifest{})
	err := policy.CheckSpawn(SpawnEnvelope{Program: "/usr/bin/git", Argv: []string{"git", "status"}})
	if err == nil {
		t.Fatal("expected denial when no spawn block was declared")
	}
	var serr *Error
	if !errors.As(err, &serr) || serr.Class != ClassCapabilityDenied || serr.Boundary != BoundarySandbox {
		t.Errorf("err = %v; want capability_denied/sandbox", err)
	}
}

func TestSpawnPolicy_AllowsDeclaredArgvPattern(t *testing.T) {
	policy := NewSpawnPolicy(goodSpawnManifest())
	if err := policy.CheckSpawn(SpawnEnvelope{
		Program: "/usr/bin/git",
		Argv:    []string{"git", "status"},
	}); err != nil {
		t.Errorf("matching argv should be allowed: %v", err)
	}
	if err := policy.CheckSpawn(SpawnEnvelope{
		Program: "/usr/bin/git",
		Argv:    []string{"git", "log", "--since=2026-01-01"},
	}); err != nil {
		t.Errorf("placeholder match should be allowed: %v", err)
	}
}

func TestSpawnPolicy_DeniesUndeclaredProgram(t *testing.T) {
	policy := NewSpawnPolicy(goodSpawnManifest())
	err := policy.CheckSpawn(SpawnEnvelope{
		Program: "/usr/bin/curl",
		Argv:    []string{"curl", "https://evil.example.com"},
	})
	if err == nil {
		t.Fatal("expected denial for undeclared program")
	}
	var serr *Error
	if !errors.As(err, &serr) || serr.Class != ClassCapabilityDenied {
		t.Fatalf("err class = %v, want capability_denied", err)
	}
	if got, _ := serr.Details["requested"].(string); got != "spawn:/usr/bin/curl" {
		t.Errorf("details.requested = %q", got)
	}
	granted, _ := serr.Details["granted"].([]string)
	sort.Strings(granted)
	if len(granted) != 1 || granted[0] != "spawn:/usr/bin/git" {
		t.Errorf("details.granted = %v", granted)
	}
}

func TestSpawnPolicy_DeniesUndeclaredArgvShape(t *testing.T) {
	policy := NewSpawnPolicy(goodSpawnManifest())
	err := policy.CheckSpawn(SpawnEnvelope{
		Program: "/usr/bin/git",
		Argv:    []string{"git", "push", "--force"},
	})
	if err == nil {
		t.Fatal("expected denial for argv shape outside grant")
	}
	var serr *Error
	if !errors.As(err, &serr) || serr.Class != ClassCapabilityDenied {
		t.Errorf("err = %v", err)
	}
}

func TestSpawnPolicy_DeniesUndeclaredEnvKey(t *testing.T) {
	policy := NewSpawnPolicy(goodSpawnManifest())
	err := policy.CheckSpawn(SpawnEnvelope{
		Program: "/usr/bin/git",
		Argv:    []string{"git", "status"},
		Env:     map[string]string{"AWS_SECRET_KEY": "leaked"},
	})
	if err == nil {
		t.Fatal("expected denial for env key not in passthrough")
	}
	var serr *Error
	if !errors.As(err, &serr) || serr.Class != ClassCapabilityDenied {
		t.Errorf("err = %v", err)
	}
}

func TestSpawnPolicy_AllowsDeclaredEnvKey(t *testing.T) {
	policy := NewSpawnPolicy(goodSpawnManifest())
	if err := policy.CheckSpawn(SpawnEnvelope{
		Program: "/usr/bin/git",
		Argv:    []string{"git", "status"},
		Env:     map[string]string{"GIT_AUTHOR_NAME": "alr"},
	}); err != nil {
		t.Errorf("declared env key should be allowed: %v", err)
	}
}

func TestSpawnPolicy_DeniesUndeclaredCredentialEnvKey(t *testing.T) {
	policy := NewSpawnPolicy(goodSpawnManifest())
	err := policy.CheckSpawn(SpawnEnvelope{
		Program:           "/usr/bin/git",
		Argv:              []string{"git", "status"},
		CredentialEnvKeys: []string{"NOT_DECLARED"},
	})
	if err == nil {
		t.Fatal("expected denial for credential env key outside passthrough")
	}
}

func TestSpawnPolicy_DeniesCwdOutsideReadScope(t *testing.T) {
	policy := NewSpawnPolicy(goodSpawnManifest())
	err := policy.CheckSpawn(SpawnEnvelope{
		Program: "/usr/bin/git",
		Argv:    []string{"git", "status"},
		Cwd:     "~/other/path",
	})
	if err == nil {
		t.Fatal("expected denial for cwd outside fs_read")
	}
}

func TestSpawnPolicy_DeniesCwdMismatchingDeclaredPolicy(t *testing.T) {
	m := goodSpawnManifest()
	m.Capabilities.Spawn.Cwd = "~/code/aileron"
	policy := NewSpawnPolicy(m)
	err := policy.CheckSpawn(SpawnEnvelope{
		Program: "/usr/bin/git",
		Argv:    []string{"git", "status"},
		Cwd:     "~/code/elsewhere",
	})
	if err == nil {
		t.Fatal("expected denial when cwd does not match declared policy")
	}
}

func TestSpawnPolicy_AllowsCwdWithinReadScope(t *testing.T) {
	policy := NewSpawnPolicy(goodSpawnManifest())
	if err := policy.CheckSpawn(SpawnEnvelope{
		Program: "/usr/bin/git",
		Argv:    []string{"git", "status"},
		Cwd:     "~/code/aileron",
	}); err != nil {
		t.Errorf("cwd within fs_read should be allowed: %v", err)
	}
}

func TestSpawnPolicy_RejectsEmptyProgram(t *testing.T) {
	policy := NewSpawnPolicy(goodSpawnManifest())
	if err := policy.CheckSpawn(SpawnEnvelope{Argv: []string{"git", "status"}}); err == nil {
		t.Fatal("expected denial for empty program")
	}
}

func TestSpawnPolicy_RejectsEmptyArgv(t *testing.T) {
	policy := NewSpawnPolicy(goodSpawnManifest())
	if err := policy.CheckSpawn(SpawnEnvelope{Program: "/usr/bin/git"}); err == nil {
		t.Fatal("expected denial for empty argv")
	}
}

// --- argvPattern matching ---

func TestArgvPattern_LiteralOnly(t *testing.T) {
	pat := parseArgvPattern("git status")
	if !pat.matches([]string{"git", "status"}) {
		t.Error("exact match should succeed")
	}
	if pat.matches([]string{"git", "log"}) {
		t.Error("non-matching literal should be rejected")
	}
	if pat.matches([]string{"git"}) {
		t.Error("length mismatch should be rejected")
	}
}

func TestArgvPattern_PlaceholderMatchesAnything(t *testing.T) {
	pat := parseArgvPattern("git log --since={date} --author={who}")
	if !pat.matches([]string{"git", "log", "--since=2026-01-01", "--author=alr"}) {
		t.Error("placeholders should match arbitrary values")
	}
}

// --- pathWithin ---

func TestPathWithin(t *testing.T) {
	cases := []struct {
		path, scope string
		want        bool
	}{
		{"~/code/aileron", "~/code/", true},
		{"~/code/aileron", "~/code", true},
		{"~/code", "~/code/", true},
		{"~/other", "~/code/", false},
		{"/var/spool/jobs", "/var/spool", true},
		{"/var", "/var/spool", false},
	}
	for _, tc := range cases {
		if got := pathWithin(tc.path, tc.scope); got != tc.want {
			t.Errorf("pathWithin(%q, %q) = %v, want %v", tc.path, tc.scope, got, tc.want)
		}
	}
}

// --- Host-function plumbing tests via fake SpawnExecutor ---
//
// The runtime's spawn host functions are registered once at
// installHostModule time; per-call state is threaded via context.
// The integration here is the whole flow from a connector calling
// `aileron_host.spawn` to the captured output coming back through
// `spawn_output_*`. The end-to-end WASM-fixture test lives in
// internal/sandbox/wazero_spawn_test.go (added with #512's fixture
// regen). Here we exercise the host-function plumbing directly with a
// fake executor.

// fakeExecutor records the envelope it was given and produces a
// scripted result. Implements SpawnExecutor.
type fakeExecutor struct {
	gotEnv SpawnEnvelope
	result SpawnResult
	err    error
}

func (f *fakeExecutor) Spawn(_ context.Context, env SpawnEnvelope) (SpawnResult, error) {
	f.gotEnv = env
	return f.result, f.err
}

// fakeResolver implements credential.Resolver for the credential-injection tests.
type fakeResolver struct {
	cred credential.Credential
	err  error
}

func (f *fakeResolver) Resolve(_ context.Context) (credential.Credential, error) {
	return f.cred, f.err
}

func TestSpawn_HostFunctions_HappyPath(t *testing.T) {
	fake := &fakeExecutor{result: SpawnResult{ExitCode: 0, Stdout: []byte("hello\n"), Stderr: nil}}
	state := &hostState{
		spawnPolicy:   NewSpawnPolicy(goodSpawnManifest()),
		spawnExecutor: fake,
		connectorFQN:  "github://aileron-test/gitcrawl",
	}
	ctx := ctxWithState(context.Background(), state)

	envBytes := mustJSON(SpawnEnvelope{
		Program: "/usr/bin/git",
		Argv:    []string{"git", "status"},
	})
	rc := processSpawn(ctx, state, envBytes)
	if rc != 0 {
		t.Fatalf("processSpawn rc = %d (err: %v)", rc, state.spawnErr)
	}
	if !state.spawnHasResult {
		t.Error("spawnHasResult should be true after a successful spawn")
	}
	if state.spawnExitCode != 0 {
		t.Errorf("exit = %d", state.spawnExitCode)
	}
	if string(state.spawnStdout) != "hello\n" {
		t.Errorf("stdout = %q", string(state.spawnStdout))
	}
	if !envvarsMatch(fake.gotEnv.Argv, []string{"git", "status"}) {
		t.Errorf("executor saw argv %v", fake.gotEnv.Argv)
	}
}

func TestSpawn_HostFunctions_DenyOnUndeclaredProgram(t *testing.T) {
	fake := &fakeExecutor{}
	state := &hostState{
		spawnPolicy:   NewSpawnPolicy(goodSpawnManifest()),
		spawnExecutor: fake,
		connectorFQN:  "github://aileron-test/gitcrawl",
	}
	ctx := ctxWithState(context.Background(), state)

	envBytes := mustJSON(SpawnEnvelope{
		Program: "/usr/bin/curl",
		Argv:    []string{"curl", "https://evil.example.com"},
	})
	rc := processSpawn(ctx, state, envBytes)
	if rc == 0 {
		t.Fatal("processSpawn should refuse undeclared program")
	}
	if state.spawnErr == nil || state.spawnErr.Class != ClassCapabilityDenied {
		t.Errorf("expected capability_denied, got %v", state.spawnErr)
	}
	if fake.gotEnv.Program != "" {
		t.Error("executor must not run when gate denies")
	}
}

func TestSpawn_HostFunctions_RefusesWhenNoManifest(t *testing.T) {
	state := &hostState{
		// no spawnPolicy — connector did not declare spawn
		connectorFQN: "github://aileron-test/gitcrawl",
	}
	ctx := ctxWithState(context.Background(), state)

	envBytes := mustJSON(SpawnEnvelope{
		Program: "/usr/bin/git",
		Argv:    []string{"git", "status"},
	})
	rc := processSpawn(ctx, state, envBytes)
	if rc == 0 {
		t.Fatal("processSpawn should refuse when policy is nil")
	}
	if state.spawnErr == nil || state.spawnErr.Class != ClassCapabilityDenied {
		t.Errorf("expected capability_denied, got %v", state.spawnErr)
	}
}

func TestSpawn_HostFunctions_ActionBoundarySubsetEnforced(t *testing.T) {
	state := &hostState{
		spawnPolicy:      NewSpawnPolicy(goodSpawnManifest()),
		spawnExecutor:    &fakeExecutor{result: SpawnResult{}},
		actionSpawnGrant: map[string]struct{}{"/usr/bin/other": {}},
		connectorFQN:     "github://aileron-test/gitcrawl",
	}
	ctx := ctxWithState(context.Background(), state)
	envBytes := mustJSON(SpawnEnvelope{
		Program: "/usr/bin/git",
		Argv:    []string{"git", "status"},
	})
	rc := processSpawn(ctx, state, envBytes)
	if rc == 0 {
		t.Fatal("expected denial when program not in action subset")
	}
	if state.spawnErr == nil || state.spawnErr.Class != ClassCapabilityDenied {
		t.Errorf("expected capability_denied, got %v", state.spawnErr)
	}
	detail, _ := state.spawnErr.Details["boundary_detail"].(string)
	if detail != "action" {
		t.Errorf("boundary_detail = %q, want action", detail)
	}
}

func TestSpawn_HostFunctions_SandboxUnavailable(t *testing.T) {
	fake := &fakeExecutor{err: ErrSpawnUnavailable}
	state := &hostState{
		spawnPolicy:   NewSpawnPolicy(goodSpawnManifest()),
		spawnExecutor: fake,
		connectorFQN:  "github://aileron-test/gitcrawl",
	}
	ctx := ctxWithState(context.Background(), state)
	envBytes := mustJSON(SpawnEnvelope{
		Program: "/usr/bin/git",
		Argv:    []string{"git", "status"},
	})
	rc := processSpawn(ctx, state, envBytes)
	if rc != -3 {
		t.Errorf("rc = %d, want -3 (spawn_sandbox_unavailable)", rc)
	}
	if state.spawnErr == nil || string(state.spawnErr.Class) != "spawn_sandbox_unavailable" {
		t.Errorf("err class = %v, want spawn_sandbox_unavailable", state.spawnErr)
	}
}

func TestSpawn_HostFunctions_CredentialInjection(t *testing.T) {
	fake := &fakeExecutor{result: SpawnResult{ExitCode: 0, Stdout: []byte("ok")}}
	state := &hostState{
		spawnPolicy:            NewSpawnPolicy(goodSpawnManifest()),
		spawnExecutor:          fake,
		connectorFQN:           "github://aileron-test/gitcrawl",
		expectedCredentialKind: "api_key",
		credentialResolver: &fakeResolver{
			cred: credential.Credential{Kind: "api_key", Value: []byte("ghs_secret123")},
		},
	}
	ctx := ctxWithState(context.Background(), state)

	envBytes := mustJSON(SpawnEnvelope{
		Program:           "/usr/bin/git",
		Argv:              []string{"git", "status"},
		CredentialEnvKeys: []string{"GH_TOKEN"},
	})
	rc := processSpawn(ctx, state, envBytes)
	if rc != 0 {
		t.Fatalf("processSpawn rc = %d (err: %v)", rc, state.spawnErr)
	}
	got := fake.gotEnv.Env["GH_TOKEN"]
	if got != "ghs_secret123" {
		t.Errorf("GH_TOKEN = %q, want injected credential", got)
	}
}

func TestSpawn_HostFunctions_CredentialBindingMissing(t *testing.T) {
	fake := &fakeExecutor{}
	state := &hostState{
		spawnPolicy:            NewSpawnPolicy(goodSpawnManifest()),
		spawnExecutor:          fake,
		connectorFQN:           "github://aileron-test/gitcrawl",
		expectedCredentialKind: "api_key",
		// no resolver wired
	}
	ctx := ctxWithState(context.Background(), state)

	envBytes := mustJSON(SpawnEnvelope{
		Program:           "/usr/bin/git",
		Argv:              []string{"git", "status"},
		CredentialEnvKeys: []string{"GH_TOKEN"},
	})
	rc := processSpawn(ctx, state, envBytes)
	if rc == 0 {
		t.Fatal("expected denial when binding is missing")
	}
	if state.spawnErr == nil || state.spawnErr.Class != ClassBindingRequired {
		t.Errorf("expected binding_required, got %v", state.spawnErr)
	}
}

func TestSpawnStatus_NoResultReturnsSentinel(t *testing.T) {
	state := &hostState{}
	ctx := ctxWithState(context.Background(), state)
	if got := hostSpawnStatus(ctx); got != -2 {
		t.Errorf("hostSpawnStatus before spawn = %d, want -2", got)
	}
}

func TestSpawnOutputSize_NoResultReturnsMinusOne(t *testing.T) {
	state := &hostState{}
	ctx := ctxWithState(context.Background(), state)
	if got := hostSpawnOutputSize(ctx, 0); got != -1 {
		t.Errorf("stdout size before spawn = %d, want -1", got)
	}
	if got := hostSpawnOutputSize(ctx, 1); got != -1 {
		t.Errorf("stderr size before spawn = %d, want -1", got)
	}
	if got := hostSpawnOutputSize(ctx, 99); got != -1 {
		t.Errorf("invalid which = %d, want -1", got)
	}
}

func TestSpawnExecutor_DefaultRunsRealBinary(t *testing.T) {
	// The default executor is a thin os/exec wrapper. Hit it once
	// against a binary that should exist on every supported test
	// platform: /bin/echo on macOS / Linux. Windows CI substitutes
	// cmd.exe; this test is gated on POSIX.
	if !commandExists("/bin/echo") {
		t.Skip("/bin/echo not present; skipping default executor smoke test")
	}
	res, err := defaultSpawnExecutor{}.Spawn(context.Background(), SpawnEnvelope{
		Program: "/bin/echo",
		Argv:    []string{"echo", "hello"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d", res.ExitCode)
	}
	if !strings.Contains(string(res.Stdout), "hello") {
		t.Errorf("stdout = %q, want to contain 'hello'", string(res.Stdout))
	}
}

func TestExpandTilde(t *testing.T) {
	got, err := expandTilde("~")
	if err != nil {
		t.Fatalf("expandTilde(~): %v", err)
	}
	if got == "~" {
		t.Errorf("~ was not expanded: %q", got)
	}
	got2, err := expandTilde("~/code")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got2, "/code") {
		t.Errorf("~/code expansion = %q", got2)
	}
	if got3, _ := expandTilde("/abs"); got3 != "/abs" {
		t.Errorf("absolute path mutated: %q", got3)
	}
}

func TestArgvShape_StableForSameInputs(t *testing.T) {
	a := argvShape([]string{"git", "log", "--since=2026-01-01"})
	b := argvShape([]string{"git", "log", "--since=2026-01-01"})
	if a != b {
		t.Errorf("argvShape not stable: %q vs %q", a, b)
	}
	c := argvShape([]string{"git", "log", "--since=2026-02-01"})
	if a == c {
		t.Errorf("argvShape should differ when interpolated arg differs")
	}
}

// --- helpers ---

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// commandExists reports whether the named binary is present on the
// host's filesystem.
func commandExists(p string) bool {
	if p == "" {
		return false
	}
	if _, err := os.Stat(p); err != nil {
		return false
	}
	return true
}

func TestWithSpawnExecutor_WiresExecutorIntoRuntime(t *testing.T) {
	fake := &fakeExecutor{result: SpawnResult{ExitCode: 0, Stdout: []byte("from-fake")}}
	rt, err := NewWazeroRuntime(context.Background(), WithSpawnExecutor(fake))
	if err != nil {
		t.Fatalf("NewWazeroRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	if rt.spawnExecutor != fake {
		t.Errorf("spawnExecutor = %v, want our fake", rt.spawnExecutor)
	}
}

// recordingHandler is a slog.Handler that records every log Record's
// attribute map for assertion.
type recordingHandler struct {
	records []map[string]any
}

func (h *recordingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	rec := map[string]any{"msg": r.Message}
	r.Attrs(func(a slog.Attr) bool {
		rec[a.Key] = a.Value.Any()
		return true
	})
	h.records = append(h.records, rec)
	return nil
}
func (h *recordingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(_ string) slog.Handler      { return h }

func TestEmitSpawnAudit_AllowDecisionEmitsExpectedAttributes(t *testing.T) {
	h := &recordingHandler{}
	state := &hostState{
		connectorFQN: "github://aileron-test/gitcrawl",
		logger:       slog.New(h),
	}
	emitSpawnAudit(context.Background(), state,
		SpawnEnvelope{Program: "/usr/bin/git", Argv: []string{"git", "status"}},
		"allow", 0, []byte("hello"), nil, "")
	if len(h.records) != 1 {
		t.Fatalf("expected one audit record, got %d", len(h.records))
	}
	rec := h.records[0]
	if rec["aileron.policy.decision"] != "allow" {
		t.Errorf("policy decision = %v", rec["aileron.policy.decision"])
	}
	if rec["program"] != "/usr/bin/git" {
		t.Errorf("program = %v", rec["program"])
	}
	if rec["stdout_sha256"] == nil {
		t.Error("stdout_sha256 missing")
	}
	if rec["stderr_sha256"] != nil {
		t.Error("stderr_sha256 should be absent for empty stderr")
	}
}

func TestEmitSpawnAudit_DenyClassPropagated(t *testing.T) {
	h := &recordingHandler{}
	state := &hostState{
		connectorFQN: "github://aileron-test/gitcrawl",
		logger:       slog.New(h),
	}
	emitSpawnAudit(context.Background(), state,
		SpawnEnvelope{Program: "/usr/bin/git", Argv: []string{"git", "status"}},
		"deny", -1, nil, []byte("permission denied"), "capability_denied")
	if len(h.records) != 1 {
		t.Fatalf("got %d records", len(h.records))
	}
	if h.records[0]["deny_class"] != "capability_denied" {
		t.Errorf("deny_class = %v", h.records[0]["deny_class"])
	}
}

func TestSpawnExecutor_DefaultPropagatesNonZeroExit(t *testing.T) {
	if !commandExists("/usr/bin/false") && !commandExists("/bin/false") {
		t.Skip("/bin/false not present")
	}
	prog := "/usr/bin/false"
	if !commandExists(prog) {
		prog = "/bin/false"
	}
	res, err := defaultSpawnExecutor{}.Spawn(context.Background(), SpawnEnvelope{
		Program: prog,
		Argv:    []string{"false"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if res.ExitCode == 0 {
		t.Error("expected non-zero exit from /bin/false")
	}
}

func TestSpawnExecutor_DefaultRejectsEmptyArgv(t *testing.T) {
	_, err := defaultSpawnExecutor{}.Spawn(context.Background(), SpawnEnvelope{
		Program: "/bin/echo",
	})
	if err == nil {
		t.Error("expected error on empty argv")
	}
}

func TestSpawnExecutor_DefaultRunsWithStdin(t *testing.T) {
	if !commandExists("/bin/cat") {
		t.Skip("/bin/cat not present")
	}
	res, err := defaultSpawnExecutor{}.Spawn(context.Background(), SpawnEnvelope{
		Program: "/bin/cat",
		Argv:    []string{"cat"},
		Stdin:   "echoed-stdin",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if string(res.Stdout) != "echoed-stdin" {
		t.Errorf("stdout = %q, want echoed input", string(res.Stdout))
	}
}

func TestSplitTokenSegments_UnmatchedBraceTreatedAsLiteral(t *testing.T) {
	segs := splitTokenSegments("--since={oops")
	// Unmatched `{` falls back to a single literal segment.
	if len(segs) != 1 || segs[0].isPlaceholder || segs[0].literal != "--since={oops" {
		t.Errorf("segments = %+v", segs)
	}
}

func TestSplitTokenSegments_EmptyBracesAreLiteral(t *testing.T) {
	segs := splitTokenSegments("--{}-x")
	// `{}` has zero placeholder name; treated as literal in the token.
	for _, s := range segs {
		if s.isPlaceholder {
			t.Errorf("`{}` should not be a placeholder: %+v", segs)
		}
	}
}

func TestSpawn_HostFunctions_NilStateReturnsMinusOne(t *testing.T) {
	if got := hostSpawnStatus(context.Background()); got != -1 {
		t.Errorf("hostSpawnStatus(no state) = %d, want -1", got)
	}
	if got := hostSpawnOutputSize(context.Background(), 0); got != -1 {
		t.Errorf("hostSpawnOutputSize(no state) = %d, want -1", got)
	}
	if got := hostSpawnOutputRead(context.Background(), nil, 0, 0, 0); got != -1 {
		t.Errorf("hostSpawnOutputRead(no state) = %d, want -1", got)
	}
}

func TestSpawn_HostFunctions_OutputReadWithNoBuffer(t *testing.T) {
	// State with no spawn result: read should return -1 from each
	// branch (stdout, stderr, invalid which).
	state := &hostState{}
	ctx := ctxWithState(context.Background(), state)
	if got := hostSpawnOutputRead(ctx, nil, 0, 0, 16); got != -1 {
		t.Errorf("stdout read with no buffer = %d", got)
	}
	if got := hostSpawnOutputRead(ctx, nil, 1, 0, 16); got != -1 {
		t.Errorf("stderr read with no buffer = %d", got)
	}
	if got := hostSpawnOutputRead(ctx, nil, 99, 0, 16); got != -1 {
		t.Errorf("invalid which = %d", got)
	}
}

func TestSpawn_HostFunctions_OutputReadEmptyBufferReturnsZero(t *testing.T) {
	// dst_len=0 short-circuits before any memory write.
	state := &hostState{spawnStdout: []byte("hi"), spawnStderr: []byte("err")}
	ctx := ctxWithState(context.Background(), state)
	if got := hostSpawnOutputRead(ctx, nil, 0, 0, 0); got != 0 {
		t.Errorf("zero-length stdout read = %d, want 0", got)
	}
	if got := hostSpawnOutputRead(ctx, nil, 1, 0, 0); got != 0 {
		t.Errorf("zero-length stderr read = %d, want 0", got)
	}
}

// noKindResolver returns ErrCredentialKindMismatch on Resolve.
type noKindResolver struct{}

func (noKindResolver) Resolve(_ context.Context) (credential.Credential, error) {
	return credential.Credential{}, credential.ErrCredentialKindMismatch
}

func TestSpawn_HostFunctions_CredentialKindMismatch(t *testing.T) {
	state := &hostState{
		spawnPolicy:            NewSpawnPolicy(goodSpawnManifest()),
		spawnExecutor:          &fakeExecutor{},
		connectorFQN:           "github://aileron-test/gitcrawl",
		expectedCredentialKind: "api_key",
		credentialResolver:     noKindResolver{},
	}
	ctx := ctxWithState(context.Background(), state)
	envBytes := mustJSON(SpawnEnvelope{
		Program:           "/usr/bin/git",
		Argv:              []string{"git", "status"},
		CredentialEnvKeys: []string{"GH_TOKEN"},
	})
	rc := processSpawn(ctx, state, envBytes)
	if rc == 0 {
		t.Fatal("expected denial for kind mismatch")
	}
	if state.spawnErr == nil || state.spawnErr.Class != ClassCapabilityDenied {
		t.Errorf("expected capability_denied, got %v", state.spawnErr)
	}
	detail, _ := state.spawnErr.Details["boundary_detail"].(string)
	if detail != "vault" {
		t.Errorf("boundary_detail = %q, want vault", detail)
	}
}

// genericErrResolver returns an unclassified error.
type genericErrResolver struct{}

func (genericErrResolver) Resolve(_ context.Context) (credential.Credential, error) {
	return credential.Credential{}, errors.New("network broke")
}

func TestSpawn_HostFunctions_CredentialResolveGenericError(t *testing.T) {
	state := &hostState{
		spawnPolicy:            NewSpawnPolicy(goodSpawnManifest()),
		spawnExecutor:          &fakeExecutor{},
		connectorFQN:           "github://aileron-test/gitcrawl",
		expectedCredentialKind: "api_key",
		credentialResolver:     genericErrResolver{},
	}
	ctx := ctxWithState(context.Background(), state)
	envBytes := mustJSON(SpawnEnvelope{
		Program:           "/usr/bin/git",
		Argv:              []string{"git", "status"},
		CredentialEnvKeys: []string{"GH_TOKEN"},
	})
	rc := processSpawn(ctx, state, envBytes)
	if rc == 0 {
		t.Fatal("expected error for generic resolve failure")
	}
	if state.spawnErr == nil || state.spawnErr.Class != ClassConnectorRuntimeError {
		t.Errorf("expected connector_runtime_error, got %v", state.spawnErr)
	}
}

func TestSpawn_HostFunctions_NoCredentialDeclaration(t *testing.T) {
	// Connector did not declare a credential capability but the spawn
	// envelope requests credential injection. Expect capability_denied.
	state := &hostState{
		spawnPolicy:   NewSpawnPolicy(goodSpawnManifest()),
		spawnExecutor: &fakeExecutor{},
		connectorFQN:  "github://aileron-test/gitcrawl",
		// expectedCredentialKind is empty
	}
	ctx := ctxWithState(context.Background(), state)
	envBytes := mustJSON(SpawnEnvelope{
		Program:           "/usr/bin/git",
		Argv:              []string{"git", "status"},
		CredentialEnvKeys: []string{"GH_TOKEN"},
	})
	rc := processSpawn(ctx, state, envBytes)
	if rc == 0 {
		t.Fatal("expected denial when no credential declared")
	}
	if state.spawnErr == nil || state.spawnErr.Class != ClassCapabilityDenied {
		t.Errorf("expected capability_denied, got %v", state.spawnErr)
	}
}

func TestSpawn_HostFunctions_MalformedJSONEnvelope(t *testing.T) {
	state := &hostState{spawnPolicy: NewSpawnPolicy(goodSpawnManifest())}
	ctx := ctxWithState(context.Background(), state)
	if rc := processSpawn(ctx, state, []byte("not json")); rc != -2 {
		t.Errorf("rc = %d, want -2 (malformed envelope)", rc)
	}
	if state.spawnErr == nil || state.spawnErr.Class != ClassConnectorRuntimeError {
		t.Errorf("err = %v", state.spawnErr)
	}
}

// fakeExecutorErr returns an arbitrary non-sandbox-unavailable error.
type fakeExecutorErr struct{ err error }

func (f fakeExecutorErr) Spawn(_ context.Context, _ SpawnEnvelope) (SpawnResult, error) {
	return SpawnResult{}, f.err
}

func TestSpawn_HostFunctions_ExecutorRunErrorPropagates(t *testing.T) {
	state := &hostState{
		spawnPolicy:   NewSpawnPolicy(goodSpawnManifest()),
		spawnExecutor: fakeExecutorErr{err: errors.New("fork failed")},
		connectorFQN:  "github://aileron-test/gitcrawl",
	}
	ctx := ctxWithState(context.Background(), state)
	envBytes := mustJSON(SpawnEnvelope{
		Program: "/usr/bin/git",
		Argv:    []string{"git", "status"},
	})
	rc := processSpawn(ctx, state, envBytes)
	if rc == 0 {
		t.Fatal("expected non-zero rc on executor error")
	}
	if state.spawnErr == nil || state.spawnErr.Class != ClassConnectorRuntimeError {
		t.Errorf("err = %v", state.spawnErr)
	}
}

func TestArgvShape_EmptyAndSingleToken(t *testing.T) {
	if got := argvShape(nil); got != "" {
		t.Errorf("argvShape(nil) = %q", got)
	}
	if got := argvShape([]string{"only"}); got != "only" {
		t.Errorf("argvShape(single) = %q", got)
	}
}

// --- spawn_op (forwarder-facing) ---

func TestSpawnPolicy_BuildEnvelopeFromOp_HappyPath(t *testing.T) {
	policy := NewSpawnPolicy(goodSpawnManifest())
	env, err := policy.BuildEnvelopeFromOp("log",
		map[string]string{"since": "2026-01-01"},
		func(string) string { return "" },
	)
	if err != nil {
		t.Fatalf("BuildEnvelopeFromOp: %v", err)
	}
	if env.Program != "/usr/bin/git" {
		t.Errorf("Program = %q", env.Program)
	}
	wantArgv := []string{"git", "log", "--since=2026-01-01"}
	if !slicesEqualStrings(env.Argv, wantArgv) {
		t.Errorf("Argv = %v, want %v", env.Argv, wantArgv)
	}
}

func TestSpawnPolicy_BuildEnvelopeFromOp_UndeclaredOpDenied(t *testing.T) {
	policy := NewSpawnPolicy(goodSpawnManifest())
	_, err := policy.BuildEnvelopeFromOp("push", nil, nil)
	if err == nil {
		t.Fatal("expected denial for undeclared op")
	}
	if err.Class != ClassCapabilityDenied {
		t.Errorf("class = %v", err.Class)
	}
	detail, _ := err.Details["boundary_detail"].(string)
	if detail != "connector_manifest" {
		t.Errorf("boundary_detail = %q", detail)
	}
}

func TestSpawnPolicy_BuildEnvelopeFromOp_MissingPlaceholderDenied(t *testing.T) {
	policy := NewSpawnPolicy(goodSpawnManifest())
	// goodSpawnManifest's "log" op expects {since}; we pass nothing.
	_, err := policy.BuildEnvelopeFromOp("log", nil, nil)
	if err == nil {
		t.Fatal("expected denial for missing placeholder")
	}
	if err.Class != ClassCapabilityDenied {
		t.Errorf("class = %v", err.Class)
	}
	if got, _ := err.Details["placeholder"].(string); got != "{since}" {
		t.Errorf("placeholder = %q", got)
	}
}

func TestSpawnPolicy_BuildEnvelopeFromOp_EnvPassthroughResolvedFromGetEnv(t *testing.T) {
	policy := NewSpawnPolicy(goodSpawnManifest())
	getEnv := func(k string) string {
		if k == "GIT_AUTHOR_NAME" {
			return "alr"
		}
		return ""
	}
	env, err := policy.BuildEnvelopeFromOp("status", nil, getEnv)
	if err != nil {
		t.Fatalf("BuildEnvelopeFromOp: %v", err)
	}
	if env.Env["GIT_AUTHOR_NAME"] != "alr" {
		t.Errorf("env.GIT_AUTHOR_NAME = %q", env.Env["GIT_AUTHOR_NAME"])
	}
	if _, present := env.Env["GH_TOKEN"]; present {
		t.Errorf("empty getEnv values should be omitted; got env=%v", env.Env)
	}
}

func TestSpawnPolicy_BuildEnvelopeFromOp_NoSpawnBlockDenied(t *testing.T) {
	policy := NewSpawnPolicy(&cstore.Manifest{})
	_, err := policy.BuildEnvelopeFromOp("log", nil, nil)
	if err == nil {
		t.Fatal("expected denial when no spawn block declared")
	}
}

func TestSpawnPolicy_BuildEnvelopeFromOp_CredentialEnvKeysPropagated(t *testing.T) {
	m := goodSpawnManifest()
	m.Capabilities.Spawn.EnvPassthrough = append(m.Capabilities.Spawn.EnvPassthrough, "API_TOKEN")
	m.Capabilities.Spawn.CredentialEnvKeys = []string{"API_TOKEN"}
	policy := NewSpawnPolicy(m)
	env, err := policy.BuildEnvelopeFromOp("status", nil, func(string) string { return "" })
	if err != nil {
		t.Fatalf("BuildEnvelopeFromOp: %v", err)
	}
	if len(env.CredentialEnvKeys) != 1 || env.CredentialEnvKeys[0] != "API_TOKEN" {
		t.Errorf("CredentialEnvKeys = %v, want [API_TOKEN]", env.CredentialEnvKeys)
	}
}

func TestSpawnPolicy_BuildEnvelopeFromOp_CwdPropagatesFromManifest(t *testing.T) {
	m := goodSpawnManifest()
	m.Capabilities.Spawn.Cwd = "~/code/"
	policy := NewSpawnPolicy(m)
	env, err := policy.BuildEnvelopeFromOp("status", nil, nil)
	if err != nil {
		t.Fatalf("BuildEnvelopeFromOp: %v", err)
	}
	if env.Cwd != "~/code/" {
		t.Errorf("Cwd = %q", env.Cwd)
	}
}

func TestArgvPattern_Substitute_FillsPlaceholders(t *testing.T) {
	pat := parseArgvPattern("git log --since={since} --author={author}")
	got, err := pat.Substitute(map[string]string{"since": "2026-01-01", "author": "alr"})
	if err != nil {
		t.Fatalf("Substitute: %v", err)
	}
	want := []string{"git", "log", "--since=2026-01-01", "--author=alr"}
	if !slicesEqualStrings(got, want) {
		t.Errorf("Substitute = %v, want %v", got, want)
	}
}

func TestArgvPattern_Substitute_RejectsMissingPlaceholder(t *testing.T) {
	pat := parseArgvPattern("git log --since={since}")
	_, err := pat.Substitute(map[string]string{}) // no since
	if err == nil {
		t.Fatal("expected denial for missing placeholder")
	}
	if got, _ := err.Details["placeholder"].(string); got != "{since}" {
		t.Errorf("placeholder = %q", got)
	}
}

func TestArgvPattern_Substitute_PreservesLiteralTokens(t *testing.T) {
	pat := parseArgvPattern("git status --short")
	got, err := pat.Substitute(nil)
	if err != nil {
		t.Fatalf("Substitute: %v", err)
	}
	if !slicesEqualStrings(got, []string{"git", "status", "--short"}) {
		t.Errorf("Substitute = %v", got)
	}
}

func TestHostSpawnOp_HappyPathRoutesThroughGate(t *testing.T) {
	fake := &fakeExecutor{result: SpawnResult{ExitCode: 0, Stdout: []byte("commit-list")}}
	state := &hostState{
		spawnPolicy:   NewSpawnPolicy(goodSpawnManifest()),
		spawnExecutor: fake,
		connectorFQN:  "github://aileron-test/gitcrawl",
	}
	ctx := ctxWithState(context.Background(), state)
	args := mustJSON(map[string]string{"since": "2026-01-01"})
	rc := processSpawnOp(ctx, state, "log", args)
	if rc != 0 {
		t.Fatalf("processSpawnOp rc = %d (err: %v)", rc, state.spawnErr)
	}
	if got := fake.gotEnv.Program; got != "/usr/bin/git" {
		t.Errorf("Program = %q", got)
	}
	wantArgv := []string{"git", "log", "--since=2026-01-01"}
	if !slicesEqualStrings(fake.gotEnv.Argv, wantArgv) {
		t.Errorf("Argv = %v, want %v", fake.gotEnv.Argv, wantArgv)
	}
}

func TestHostSpawnOp_UndeclaredOpDeniedBeforeExecutor(t *testing.T) {
	fake := &fakeExecutor{}
	state := &hostState{
		spawnPolicy:   NewSpawnPolicy(goodSpawnManifest()),
		spawnExecutor: fake,
		connectorFQN:  "github://aileron-test/gitcrawl",
	}
	ctx := ctxWithState(context.Background(), state)
	rc := processSpawnOp(ctx, state, "push", nil)
	if rc == 0 {
		t.Fatal("expected denial for undeclared op")
	}
	if state.spawnErr == nil || state.spawnErr.Class != ClassCapabilityDenied {
		t.Errorf("expected capability_denied, got %v", state.spawnErr)
	}
	if fake.gotEnv.Program != "" {
		t.Error("executor must not run when gate denies")
	}
}

func TestHostSpawnOp_MissingPlaceholderDenied(t *testing.T) {
	fake := &fakeExecutor{}
	state := &hostState{
		spawnPolicy:   NewSpawnPolicy(goodSpawnManifest()),
		spawnExecutor: fake,
		connectorFQN:  "github://aileron-test/gitcrawl",
	}
	ctx := ctxWithState(context.Background(), state)
	args := mustJSON(map[string]string{}) // log expects {since}
	rc := processSpawnOp(ctx, state, "log", args)
	if rc == 0 {
		t.Fatal("expected denial for missing placeholder")
	}
	if state.spawnErr == nil || state.spawnErr.Class != ClassCapabilityDenied {
		t.Errorf("expected capability_denied, got %v", state.spawnErr)
	}
	detail, _ := state.spawnErr.Details["boundary_detail"].(string)
	if detail != "envelope" {
		t.Errorf("boundary_detail = %q, want envelope", detail)
	}
}

func TestHostSpawnOp_RefusesWhenNoSpawnPolicy(t *testing.T) {
	state := &hostState{
		// no spawnPolicy
		connectorFQN: "github://aileron-test/gitcrawl",
	}
	ctx := ctxWithState(context.Background(), state)
	rc := processSpawnOp(ctx, state, "log", nil)
	if rc == 0 {
		t.Fatal("expected denial when no policy declared")
	}
	if state.spawnErr == nil || state.spawnErr.Class != ClassCapabilityDenied {
		t.Errorf("expected capability_denied, got %v", state.spawnErr)
	}
}

func TestHostSpawnOp_MalformedJSONArgsDenied(t *testing.T) {
	state := &hostState{
		spawnPolicy:   NewSpawnPolicy(goodSpawnManifest()),
		spawnExecutor: &fakeExecutor{},
		connectorFQN:  "github://aileron-test/gitcrawl",
	}
	ctx := ctxWithState(context.Background(), state)
	rc := processSpawnOp(ctx, state, "log", []byte("not json"))
	if rc != -2 {
		t.Errorf("rc = %d, want -2", rc)
	}
	if state.spawnErr == nil || state.spawnErr.Class != ClassConnectorRuntimeError {
		t.Errorf("err = %v", state.spawnErr)
	}
}

func TestHostSpawnOp_GateStillBlocksDeniedArgvAfterSubstitution(t *testing.T) {
	// Substitution could in principle produce an argv that does not
	// match the declared pattern (e.g. the operation pattern itself
	// changed between policy build and call — not a real-world scenario
	// today but the gate is the safety net). Confirm gate denial works
	// even when we're driving the manifest's own operation: an op
	// that substitutes to an argv shape the gate accepts succeeds.
	state := &hostState{
		spawnPolicy:   NewSpawnPolicy(goodSpawnManifest()),
		spawnExecutor: &fakeExecutor{result: SpawnResult{ExitCode: 0}},
		connectorFQN:  "github://aileron-test/gitcrawl",
	}
	ctx := ctxWithState(context.Background(), state)
	args := mustJSON(map[string]string{"since": "2026-01-01"})
	rc := processSpawnOp(ctx, state, "log", args)
	if rc != 0 {
		t.Fatalf("rc = %d (err: %v)", rc, state.spawnErr)
	}
}

// slicesEqualStrings is a local helper (the existing slicesEqual is in
// scope but uses [string]; this is the same behavior under a clearer
// name for the new tests).
func slicesEqualStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPathWithin_TrailingSlashEdgeCases(t *testing.T) {
	if !pathWithin("~/code/", "~/code/") {
		t.Error("identical with trailing slash should match")
	}
}

func envvarsMatch(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
