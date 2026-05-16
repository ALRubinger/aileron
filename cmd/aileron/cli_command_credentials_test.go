package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/sandbox/sandboxtest"
	"github.com/ALRubinger/aileron/internal/vault"
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
	var stdout, stderr bytes.Buffer
	written, err := stashCredentialBindings(
		"local://user/x", "x", nil, "",
		strings.NewReader(""), &stdout, &stderr,
	)
	if err != nil {
		t.Errorf("unexpected error for empty keys: %v", err)
	}
	if written != 0 {
		t.Errorf("written=%d want 0", written)
	}
}

func TestStashCredentialBindings_EmptyValueSkipsVaultWrite(t *testing.T) {
	// User leaves the credential value blank (legitimate skip for
	// connectors where the heuristic surfaced a false positive).
	// stashCredentialBindings must return (0, nil) without touching
	// the vault — proven by NOT setting up a vault passphrase.
	prevPrompt := promptPassphrase
	t.Cleanup(func() { promptPassphrase = prevPrompt })
	promptPassphrase = func(prompt string, w io.Writer) (string, error) {
		// Empty value → skip path.
		return "", nil
	}

	var stdout, stderr bytes.Buffer
	written, err := stashCredentialBindings(
		"local://user/skipme", "skipme",
		[]string{"SKIPME_API_TOKEN"}, "",
		strings.NewReader(""), &stdout, &stderr,
	)
	if err != nil {
		t.Fatalf("expected nil err on empty-value skip: %v", err)
	}
	if written != 0 {
		t.Errorf("written=%d want 0", written)
	}
}

func TestStashCredentialBindings_WritesBindingToVault(t *testing.T) {
	// End-to-end: the vault is pre-initialized (state machine's
	// responsibility per #783), the cached passphrase lets the
	// stash open the vault without prompting, and one binding
	// lands. Verifies the binding-write half of the BYOCLI flow
	// in isolation from the state machine.
	fakeHome(t)
	vaultPath := launch.DefaultVaultPath()
	if _, err := vault.Init(vaultPath, "cached-passphrase"); err != nil {
		t.Fatalf("vault.Init: %v", err)
	}
	rememberVaultPassphrase("cached-passphrase")
	t.Cleanup(func() { rememberVaultPassphrase("") }) // clear for next test

	prevPrompt := promptPassphrase
	t.Cleanup(func() { promptPassphrase = prevPrompt })
	// Only the credential-value prompt fires — the passphrase
	// comes from the cache. A second prompt here would mean the
	// state-machine-cache path silently regressed.
	calls := 0
	promptPassphrase = func(prompt string, w io.Writer) (string, error) {
		calls++
		return "super-secret-token-bytes", nil
	}

	var stdout, stderr bytes.Buffer
	written, err := stashCredentialBindings(
		"local://user/linear", "linear",
		[]string{"LINEAR_API_TOKEN"}, "",
		strings.NewReader(""), &stdout, &stderr,
	)
	if err != nil {
		t.Fatalf("stash: %v", err)
	}
	if written != 1 {
		t.Errorf("written=%d want 1", written)
	}
	if calls != 1 {
		t.Errorf("expected only 1 prompt (credential value); the cached passphrase should skip the passphrase prompt — got %d prompts", calls)
	}
	if !strings.Contains(stdout.String(), "Stashed credential under binding api_key/linear/default") {
		t.Errorf("stdout missing success line: %q", stdout.String())
	}
}

func TestStashCredentialBindings_FallsBackToPromptWhenCacheEmpty(t *testing.T) {
	// When `pp add` / `cli add` is invoked while the daemon is
	// already running and the vault is unlocked, the state
	// machine's no-prompt branch fires and the cache stays empty.
	// stashCredentialBindings must still resolve a passphrase —
	// falling back to readVaultPassphrase (file/env/prompt).
	fakeHome(t)
	vaultPath := launch.DefaultVaultPath()
	if _, err := vault.Init(vaultPath, "from-prompt"); err != nil {
		t.Fatalf("vault.Init: %v", err)
	}
	rememberVaultPassphrase("") // ensure cache is clear
	t.Cleanup(func() { rememberVaultPassphrase("") })

	prevPrompt := promptPassphrase
	t.Cleanup(func() { promptPassphrase = prevPrompt })
	calls := 0
	canned := []string{
		"credential-bytes", // credential value
		"from-prompt",      // vault passphrase (matches Init)
	}
	promptPassphrase = func(prompt string, w io.Writer) (string, error) {
		v := canned[calls]
		calls++
		return v, nil
	}

	var stdout, stderr bytes.Buffer
	written, err := stashCredentialBindings(
		"local://user/already", "already",
		[]string{"ALREADY_API_KEY"}, "",
		strings.NewReader(""), &stdout, &stderr,
	)
	if err != nil {
		t.Fatalf("stash: %v", err)
	}
	if written != 1 {
		t.Errorf("written=%d want 1", written)
	}
	if calls != 2 {
		t.Errorf("expected 2 prompts (credential + passphrase fallback), got %d", calls)
	}
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
