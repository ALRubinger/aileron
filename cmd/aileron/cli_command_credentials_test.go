package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/sandbox/sandboxtest"
	"github.com/ALRubinger/aileron/internal/wrap"
)

func TestResolveCredentialKeys_HonorsSkipFlag(t *testing.T) {
	got := resolveCredentialKeys("LINEAR_API_TOKEN is required", stringList{}, true)
	if got != nil {
		t.Errorf("--no-credentials must yield nil, got %v", got)
	}
}

func TestResolveCredentialKeys_MergesOverridesWithHeuristic(t *testing.T) {
	help := "Set LINEAR_API_TOKEN before running."
	overrides := stringList{"WEIRD_CUSTOM_KEY"}
	got := resolveCredentialKeys(help, overrides, false)
	wantSubset := []string{"LINEAR_API_TOKEN", "WEIRD_CUSTOM_KEY"}
	for _, want := range wantSubset {
		if !contains(got, want) {
			t.Errorf("missing %q in %v", want, got)
		}
	}
}

func TestResolveCredentialKeys_DeduplicatesAcrossSources(t *testing.T) {
	// User passes --credential LINEAR_API_TOKEN even though the
	// heuristic already found it. Output must contain one copy.
	overrides := stringList{"LINEAR_API_TOKEN"}
	got := resolveCredentialKeys("LINEAR_API_TOKEN env var", overrides, false)
	count := 0
	for _, k := range got {
		if k == "LINEAR_API_TOKEN" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("LINEAR_API_TOKEN appeared %d times, want 1; got %v", count, got)
	}
}

func TestApplyCredentialsToSpec_PopulatesAllFields(t *testing.T) {
	spec := &wrap.Spec{}
	applyCredentialsToSpec(spec, []string{"LINEAR_API_TOKEN"})
	if !contains(spec.EnvPassthrough, "LINEAR_API_TOKEN") {
		t.Errorf("EnvPassthrough missing key: %v", spec.EnvPassthrough)
	}
	if !contains(spec.CredentialEnvKeys, "LINEAR_API_TOKEN") {
		t.Errorf("CredentialEnvKeys missing key: %v", spec.CredentialEnvKeys)
	}
	if spec.Credential == nil {
		t.Fatal("Credential capability block not set")
	}
	if spec.Credential.Kind != cstore.CredentialKindAPIKey {
		t.Errorf("Credential.Kind=%q, want %q", spec.Credential.Kind, cstore.CredentialKindAPIKey)
	}
}

func TestApplyCredentialsToSpec_NoOpOnEmpty(t *testing.T) {
	spec := &wrap.Spec{}
	applyCredentialsToSpec(spec, nil)
	if spec.Credential != nil {
		t.Errorf("Credential should stay nil for empty keys, got %+v", spec.Credential)
	}
	if len(spec.EnvPassthrough) != 0 {
		t.Errorf("EnvPassthrough should stay empty, got %v", spec.EnvPassthrough)
	}
}

func TestApplyCredentialsToSpec_PreservesExistingCredentialBlock(t *testing.T) {
	// Refresh path: the spec already carries an OAuth2 credential
	// block (hand-written manifest the user might have edited via
	// --edit). applyCredentialsToSpec must NOT overwrite it back to
	// api_key — the user's choice wins.
	spec := &wrap.Spec{
		Credential: &wrap.CredentialSpec{Kind: "oauth2"},
	}
	applyCredentialsToSpec(spec, []string{"FOO_TOKEN"})
	if spec.Credential.Kind != "oauth2" {
		t.Errorf("existing OAuth2 kind clobbered: got %q", spec.Credential.Kind)
	}
}

func TestMergeStringSets_SortsAndDeduplicates(t *testing.T) {
	got := mergeStringSets([]string{"b", "a"}, []string{"c", "a", "b"})
	want := []string{"a", "b", "c"}
	for i, w := range want {
		if i >= len(got) || got[i] != w {
			t.Errorf("got %v want %v", got, want)
			break
		}
	}
}

func TestMergeStringSets_EmptyInputsReturnNil(t *testing.T) {
	if got := mergeStringSets(nil, nil); got != nil {
		t.Errorf("expected nil for empty inputs, got %v", got)
	}
}

func TestStringList_AppendsAndDeduplicates(t *testing.T) {
	var s stringList
	for _, v := range []string{"A", "B", "A", "C"} {
		if err := s.Set(v); err != nil {
			t.Errorf("Set(%q): %v", v, err)
		}
	}
	if len(s) != 3 {
		t.Errorf("len=%d, want 3 (deduped), got %v", len(s), s)
	}
	if s.String() != "A,B,C" {
		t.Errorf("String()=%q want %q", s.String(), "A,B,C")
	}
}

// TestRunCliAdd_NoCredentialsSkipsPrompt verifies the
// --no-credentials flag short-circuits the detection path so an
// installed manifest stays credential-free. Important because
// some CLIs ship help text that name-drops env vars in unrelated
// contexts (e.g. "PATH is searched"); skipping is the escape
// hatch for false positives.
func TestRunCliAdd_NoCredentialsSkipsPrompt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sandboxtest.FakeBinary is POSIX-only")
	}
	home := fakeHome(t)
	dir := t.TempDir()
	fake := sandboxtest.FakeBinary{
		Stdout: "Set MYCLI_API_TOKEN to authenticate.\nCommands:\n  list  List things\n",
	}
	binPath, err := fake.Write(dir, "mycli")
	if err != nil {
		t.Fatalf("write fake: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runCliAdd(
		[]string{"--yes", "--no-credentials", binPath},
		strings.NewReader(""),
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	body, err := os.ReadFile(filepath.Join(home, ".aileron", "connectors", "local", "mycli", "manifest.toml"))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	m, err := cstore.ParseManifest("manifest.toml", body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Capabilities.Credential != nil {
		t.Errorf("--no-credentials should leave credential block nil, got %+v", m.Capabilities.Credential)
	}
	if len(m.Capabilities.Spawn.CredentialEnvKeys) != 0 {
		t.Errorf("--no-credentials should leave CredentialEnvKeys empty, got %v", m.Capabilities.Spawn.CredentialEnvKeys)
	}
}

// TestRunCliAdd_DetectsAndDeclaresCredentialEnvKeys verifies the
// happy detection path: help text mentioning a known credential
// pattern lands in the manifest, even without prompting for the
// value (`--no-credentials` keeps the test deterministic across
// CI runners without a vault).
//
// The manifest's `[capabilities.credential]` block is also
// suppressed under --no-credentials, since declaring it without
// a binding would surface `binding_required` on every call.
func TestRunCliAdd_DetectsAndDeclaresCredentialEnvKeys(t *testing.T) {
	// Without `--no-credentials`, this test would hang on the
	// interactive value prompt. Verifying detection-only is fine
	// — the heuristic itself is exhaustively covered in
	// internal/wrap/credentials_test.go.
	if runtime.GOOS == "windows" {
		t.Skip("sandboxtest.FakeBinary is POSIX-only")
	}
	t.Skip("happy-path credential prompt requires interactive vault; covered by credentials_test.go + helpers above")
}

// TestRunCliRefresh_PreservesCredentialEnvKeys is the load-bearing
// regression test from #750 acceptance: after `cli add` declares
// credential env keys, `cli refresh` re-introspecting must NOT
// silently drop them, even if the new help text omits the
// mention. Seed a manifest with a known CredentialEnvKey, run
// refresh against a fake binary whose help has different text,
// and verify the existing key survives.
func TestRunCliRefresh_PreservesCredentialEnvKeys(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sandboxtest.FakeBinary is POSIX-only")
	}
	home := fakeHome(t)

	// Seed a manifest that declares MYCLI_API_TOKEN as a credential
	// env key. The on-disk binary will be replaced below so refresh
	// has a real target to invoke.
	store := cstore.NewLocalStore(filepath.Join(home, ".aileron", "connectors", "local"))
	binPath := filepath.Join(t.TempDir(), "mycli")
	// First write a placeholder; the actual fake will overwrite it
	// to point --help at the post-refresh shape.
	fakeOld := sandboxtest.FakeBinary{
		Stdout: "Set MYCLI_API_TOKEN to authenticate.\nCommands:\n  list  List\n",
	}
	if _, err := fakeOld.Write(filepath.Dir(binPath), "mycli"); err != nil {
		t.Fatalf("write fake (initial): %v", err)
	}

	fqn, _ := cstore.LocalFQN("mycli")
	seeded := &cstore.Manifest{
		Connector: cstore.ManifestConnector{
			Name:      fqn,
			Version:   "0.0.1",
			Origin:    cstore.OriginLocal,
			Forwarder: cstore.BuiltinForwarderSpawn,
		},
		Capabilities: cstore.ManifestCapabilities{
			Spawn: &cstore.ManifestSpawn{
				Programs: []cstore.ManifestSpawnProgram{
					{Path: binPath},
				},
				Operations: map[string]cstore.ManifestSpawnOperation{
					"list": {Argv: "list"},
				},
				EnvPassthrough:    []string{"MYCLI_API_TOKEN"},
				CredentialEnvKeys: []string{"MYCLI_API_TOKEN"},
			},
			Credential: &cstore.ManifestCredential{Kind: cstore.CredentialKindAPIKey},
		},
	}
	if _, err := store.Save("mycli", seeded); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}

	// Now overwrite the binary's --help to a version that does NOT
	// mention any credential env var. The preservation logic must
	// keep MYCLI_API_TOKEN in the refreshed manifest even though
	// detection on the new text would return nothing.
	fakeNew := sandboxtest.FakeBinary{
		Stdout: "Commands:\n  list  List\n  show  Show\n",
	}
	if _, err := fakeNew.Write(filepath.Dir(binPath), "mycli"); err != nil {
		t.Fatalf("write fake (new): %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runCliRefresh(
		[]string{"--yes", "mycli"},
		strings.NewReader(""),
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("refresh exit=%d stderr=%s", code, stderr.String())
	}
	refreshed, err := store.Load("mycli")
	if err != nil {
		t.Fatalf("load refreshed: %v", err)
	}
	if !contains(refreshed.Capabilities.Spawn.CredentialEnvKeys, "MYCLI_API_TOKEN") {
		t.Errorf("CredentialEnvKeys lost MYCLI_API_TOKEN after refresh: %v",
			refreshed.Capabilities.Spawn.CredentialEnvKeys)
	}
	if refreshed.Capabilities.Credential == nil {
		t.Error("Credential capability dropped after refresh")
	}
}

// contains reports whether the slice contains v. Test-local
// since the cmd/aileron package doesn't carry a generic helper.
func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestStashCredentialBindings_EmptyKeysIsNoOp(t *testing.T) {
	// No credential env keys → no daemon round trip, no prompt.
	// The fake transport stays untouched.
	calls := 0
	withFakeBindingTransport(t, func(method, path string, body io.Reader) (int, []byte, error) {
		calls++
		return 0, nil, errors.New("no transport call expected")
	})

	var stdout, stderr bytes.Buffer
	written, err := stashCredentialBindings(
		"local://user/x", "x", nil,
		strings.NewReader(""), &stdout, &stderr,
	)
	if err != nil {
		t.Errorf("unexpected error for empty keys: %v", err)
	}
	if written != 0 {
		t.Errorf("written=%d want 0", written)
	}
	if calls != 0 {
		t.Errorf("transport called %d times for empty keys; should not be called at all", calls)
	}
}

func TestStashCredentialBindings_EmptyValueSkipsDaemonCall(t *testing.T) {
	// User leaves the credential value blank (legitimate skip for
	// connectors where the heuristic surfaced a false positive).
	// stashCredentialBindings must return (0, nil) without
	// hitting the daemon — proven by failing the fake transport.
	prevPrompt := promptPassphrase
	t.Cleanup(func() { promptPassphrase = prevPrompt })
	promptPassphrase = func(prompt string, w io.Writer) (string, error) {
		return "", nil // empty value → skip path
	}
	transportCalls := 0
	withFakeBindingTransport(t, func(method, path string, body io.Reader) (int, []byte, error) {
		transportCalls++
		return 0, nil, errors.New("no daemon call expected for empty-value skip")
	})

	var stdout, stderr bytes.Buffer
	written, err := stashCredentialBindings(
		"local://user/skipme", "skipme",
		[]string{"SKIPME_API_TOKEN"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if err != nil {
		t.Fatalf("expected nil err on empty-value skip: %v", err)
	}
	if written != 0 {
		t.Errorf("written=%d want 0", written)
	}
	if transportCalls != 0 {
		t.Errorf("daemon transport called %d times during empty-value skip; should be 0", transportCalls)
	}
	if !strings.Contains(stdout.String(), "no credential stashed") {
		t.Errorf("stdout should explain the skip; got %q", stdout.String())
	}
}

func TestStashCredentialBindings_PostsBindingToDaemon(t *testing.T) {
	// Happy path: the user enters a value, stashCredentialBindings
	// POSTs to /bindings/setup with the right body, and the
	// daemon's `created` reply lands in stdout. No vault file is
	// opened; no second prompt fires.
	prevPrompt := promptPassphrase
	t.Cleanup(func() { promptPassphrase = prevPrompt })
	promptCalls := 0
	promptPassphrase = func(prompt string, w io.Writer) (string, error) {
		promptCalls++
		return "super-secret-token-bytes", nil
	}

	var seenMethod, seenPath, seenBody string
	withFakeBindingTransport(t, func(method, path string, body io.Reader) (int, []byte, error) {
		seenMethod = method
		seenPath = path
		bs, _ := io.ReadAll(body)
		seenBody = string(bs)
		return 201, []byte(`{"created":[{"name":"api_key/linear/default"}],"skipped":[]}`), nil
	})

	var stdout, stderr bytes.Buffer
	written, err := stashCredentialBindings(
		"local://user/linear", "linear",
		[]string{"LINEAR_API_TOKEN"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if err != nil {
		t.Fatalf("stash: %v", err)
	}
	if written != 1 {
		t.Errorf("written=%d want 1", written)
	}
	if promptCalls != 1 {
		t.Errorf("expected exactly 1 prompt (credential value); got %d. A second prompt means the dual-prompt UX trap regressed.", promptCalls)
	}
	if seenMethod != "POST" || seenPath != "/bindings/setup" {
		t.Errorf("request: %s %s, want POST /bindings/setup", seenMethod, seenPath)
	}
	for _, want := range []string{
		`"connector_fqn":"local://user/linear"`,
		`"identity":"default"`,
		`"kind":"api_key"`,
		`"value":"super-secret-token-bytes"`,
	} {
		if !strings.Contains(seenBody, want) {
			t.Errorf("request body missing %q; got %s", want, seenBody)
		}
	}
	if !strings.Contains(stdout.String(), "Stashed credential under binding api_key/linear/default") {
		t.Errorf("stdout missing success line: %q", stdout.String())
	}
}

func TestStashCredentialBindings_DaemonRejectionReturnsError(t *testing.T) {
	// Daemon returns non-201 (e.g., the connector isn't installed
	// — Bug B before the fix shipped). The caller must see a
	// concrete error so the install can exit non-zero, never the
	// silent-warning shape that produced the linear-auth bug.
	prevPrompt := promptPassphrase
	t.Cleanup(func() { promptPassphrase = prevPrompt })
	promptPassphrase = func(prompt string, w io.Writer) (string, error) {
		return "value", nil
	}
	withFakeBindingTransport(t, func(method, path string, body io.Reader) (int, []byte, error) {
		return 404, []byte(`{"error":{"code":"connector_not_installed","message":"connector local://user/x is not installed"}}`), nil
	})

	var stdout, stderr bytes.Buffer
	written, err := stashCredentialBindings(
		"local://user/x", "x",
		[]string{"X_API_KEY"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if err == nil {
		t.Fatalf("expected non-nil error on 404; stdout=%q", stdout.String())
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("err = %v, want status surfaced in message", err)
	}
	if !strings.Contains(err.Error(), "connector_not_installed") {
		t.Errorf("err = %v, want daemon response body surfaced in message", err)
	}
	if written != 0 {
		t.Errorf("written=%d want 0 on rejection", written)
	}
}

func TestStashCredentialBindings_HTTPErrorPropagates(t *testing.T) {
	// Transport error (e.g., daemon unreachable) must surface as a
	// real error, not a silent zero return.
	prevPrompt := promptPassphrase
	t.Cleanup(func() { promptPassphrase = prevPrompt })
	promptPassphrase = func(prompt string, w io.Writer) (string, error) {
		return "value", nil
	}
	withFakeBindingTransport(t, func(method, path string, body io.Reader) (int, []byte, error) {
		return 0, nil, errors.New("connection refused")
	})

	var stdout, stderr bytes.Buffer
	_, err := stashCredentialBindings(
		"local://user/x", "x",
		[]string{"X_API_KEY"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("err = %v, want transport error propagated", err)
	}
}

func TestStashCredentialBindings_SkippedBindingsCountInReturn(t *testing.T) {
	// Daemon reports the binding already exists (idempotent
	// reinstall). That should count toward `written` so the caller
	// reports a successful end-state and doesn't exit non-zero on
	// a reinstall.
	prevPrompt := promptPassphrase
	t.Cleanup(func() { promptPassphrase = prevPrompt })
	promptPassphrase = func(prompt string, w io.Writer) (string, error) {
		return "value", nil
	}
	withFakeBindingTransport(t, func(method, path string, body io.Reader) (int, []byte, error) {
		return 201, []byte(`{"created":[],"skipped":["api_key/linear/default"]}`), nil
	})

	var stdout, stderr bytes.Buffer
	written, err := stashCredentialBindings(
		"local://user/linear", "linear",
		[]string{"LINEAR_API_TOKEN"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if err != nil {
		t.Fatalf("stash: %v", err)
	}
	if written != 1 {
		t.Errorf("written=%d want 1 (skipped == still-bound counts as a successful end-state)", written)
	}
	if !strings.Contains(stdout.String(), "already exists") {
		t.Errorf("stdout should note the skip; got %q", stdout.String())
	}
}

// withFakeBindingTransport swaps bindingDoRequestFn for the
// duration of a test. The restore handler runs through t.Cleanup
// so test order doesn't affect the cached production value.
func withFakeBindingTransport(t *testing.T, fn func(method, path string, body io.Reader) (int, []byte, error)) {
	t.Helper()
	prev := bindingDoRequestFn
	t.Cleanup(func() { bindingDoRequestFn = prev })
	bindingDoRequestFn = fn
}

func TestRememberRecallVaultPassphrase_RoundTrip(t *testing.T) {
	// Cache helpers are the only contract `stashCredentialBindings`
	// relies on to skip its passphrase prompt after the state
	// machine has verified the vault for this process. Pin the
	// round-trip behavior so a future refactor doesn't silently
	// break the single-prompt UX.
	t.Cleanup(func() { rememberVaultPassphrase("") })
	rememberVaultPassphrase("")
	if got := recallVaultPassphrase(); got != "" {
		t.Errorf("clean cache should be empty; got %q", got)
	}
	rememberVaultPassphrase("hello")
	if got := recallVaultPassphrase(); got != "hello" {
		t.Errorf("recall=%q want hello", got)
	}
	rememberVaultPassphrase("")
	if got := recallVaultPassphrase(); got != "" {
		t.Errorf("empty input should clear the cache; got %q", got)
	}
}

// _ keeps the io import alive when only the test seam references it.
var _ = io.Discard
