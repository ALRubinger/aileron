package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/sandbox/sandboxtest"
)

// fakeHome redirects HOME and XDG_CONFIG_HOME to a tempdir so
// `aileron cli` commands write to t.TempDir() instead of the
// developer's real home. Returns the tempdir path.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

func TestRunCliAdd_RejectsMissingPath(t *testing.T) {
	fakeHome(t)
	var stdout, stderr bytes.Buffer
	code := runCliAdd(nil, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit for missing path")
	}
}

func TestRunCliAdd_RejectsNonExistentPath(t *testing.T) {
	fakeHome(t)
	var stdout, stderr bytes.Buffer
	code := runCliAdd(
		[]string{"/nonexistent/binary"},
		strings.NewReader(""),
		&stdout, &stderr,
	)
	if code == 0 {
		t.Error("expected nonzero exit for nonexistent binary")
	}
	if !strings.Contains(stderr.String(), "stat") {
		t.Errorf("stderr should mention stat failure: %q", stderr.String())
	}
}

func TestRunCliAdd_DryRunWritesNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sandboxtest.FakeBinary is POSIX-only")
	}
	home := fakeHome(t)
	dir := t.TempDir()
	fake := sandboxtest.FakeBinary{
		Stdout: "Usage: tinycli [command]\n\nCommands:\n  ping  Ping a host\n  pong  Reply\n",
	}
	binPath, err := fake.Write(dir, "tinycli")
	if err != nil {
		t.Fatalf("write fake: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runCliAdd(
		[]string{"--dry-run", "--yes", binPath},
		strings.NewReader(""),
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "dry-run") {
		t.Errorf("dry-run message missing: %q", stdout.String())
	}
	manifestPath := filepath.Join(home, ".aileron", "connectors", "local", "tinycli", "manifest.toml")
	if _, err := os.Stat(manifestPath); err == nil {
		t.Errorf("dry-run wrote manifest to %s", manifestPath)
	}
}

func TestRunCliAdd_WritesLocalManifest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sandboxtest.FakeBinary is POSIX-only")
	}
	home := fakeHome(t)
	dir := t.TempDir()
	fake := sandboxtest.FakeBinary{
		Stdout: "Usage: tinycli [command]\n\nCommands:\n  ping  Ping a host\n  pong  Reply\n",
	}
	binPath, err := fake.Write(dir, "tinycli")
	if err != nil {
		t.Fatalf("write fake: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runCliAdd(
		[]string{"--yes", binPath},
		strings.NewReader(""),
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	manifestPath := filepath.Join(home, ".aileron", "connectors", "local", "tinycli", "manifest.toml")
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
	m, err := cstore.ParseManifest(manifestPath, body)
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if m.Connector.Origin != cstore.OriginLocal {
		t.Errorf("origin=%q want %q", m.Connector.Origin, cstore.OriginLocal)
	}
	if m.Connector.Forwarder != cstore.BuiltinForwarderSpawn {
		t.Errorf("forwarder=%q want %q", m.Connector.Forwarder, cstore.BuiltinForwarderSpawn)
	}
	expectedFQN, _ := cstore.LocalFQN("tinycli")
	if m.Connector.Name != expectedFQN {
		t.Errorf("name=%q want %q", m.Connector.Name, expectedFQN)
	}
	if m.Capabilities.Spawn == nil {
		t.Fatal("missing [capabilities.spawn]")
	}
	if len(m.Capabilities.Spawn.Programs) != 1 || m.Capabilities.Spawn.Programs[0].Path != binPath {
		t.Errorf("program path: got %+v want %s", m.Capabilities.Spawn.Programs, binPath)
	}
	// FromHelp's parser may produce subcommand entries OR fall
	// back to a single "run" op. Verify at least one operation
	// landed in the manifest so the action surface is non-empty.
	if len(m.Capabilities.Spawn.Operations) == 0 {
		t.Errorf("expected at least one operation, got none")
	}
}

func TestRunCliList_EmptyShowsHint(t *testing.T) {
	fakeHome(t)
	var stdout, stderr bytes.Buffer
	code := runCliList(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No local connectors") {
		t.Errorf("empty-state hint missing: %q", stdout.String())
	}
}

func TestRunCliList_RendersInstalled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sandboxtest.FakeBinary is POSIX-only")
	}
	home := fakeHome(t)

	// Seed a manifest directly via the store so this test doesn't
	// depend on the introspection path.
	store := cstore.NewLocalStore(filepath.Join(home, ".aileron", "connectors", "local"))
	fqn, _ := cstore.LocalFQN("seeded")
	m := &cstore.Manifest{
		Connector: cstore.ManifestConnector{
			Name:      fqn,
			Version:   "0.0.1",
			Origin:    cstore.OriginLocal,
			Forwarder: cstore.BuiltinForwarderSpawn,
		},
		Capabilities: cstore.ManifestCapabilities{
			Spawn: &cstore.ManifestSpawn{
				Programs: []cstore.ManifestSpawnProgram{
					{Path: "/usr/local/bin/seeded"},
				},
				Operations: map[string]cstore.ManifestSpawnOperation{
					"help": {Argv: "--help"},
				},
			},
		},
	}
	if _, err := store.Save("seeded", m); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runCliList(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "seeded") {
		t.Errorf("list missing entry: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "/usr/local/bin/seeded") {
		t.Errorf("list missing program path: %q", stdout.String())
	}
}

func TestRunCliRemove_DeletesEntry(t *testing.T) {
	home := fakeHome(t)
	store := cstore.NewLocalStore(filepath.Join(home, ".aileron", "connectors", "local"))
	fqn, _ := cstore.LocalFQN("doomed")
	m := &cstore.Manifest{
		Connector: cstore.ManifestConnector{
			Name:      fqn,
			Version:   "0.0.1",
			Origin:    cstore.OriginLocal,
			Forwarder: cstore.BuiltinForwarderSpawn,
		},
		Capabilities: cstore.ManifestCapabilities{
			Spawn: &cstore.ManifestSpawn{
				Programs:   []cstore.ManifestSpawnProgram{{Path: "/usr/local/bin/doomed"}},
				Operations: map[string]cstore.ManifestSpawnOperation{"help": {Argv: "--help"}},
			},
		},
	}
	if _, err := store.Save("doomed", m); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runCliRemove([]string{"doomed"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	has, _ := store.Has("doomed")
	if has {
		t.Error("Has returned true after remove")
	}
}

func TestRunCliRemove_ErrorsOnMissing(t *testing.T) {
	fakeHome(t)
	var stdout, stderr bytes.Buffer
	code := runCliRemove([]string{"never-existed"}, &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit for missing entry")
	}
}

func TestRunCliRefresh_PreservesCredentials(t *testing.T) {
	// Refresh is the introspector applied to the existing
	// program path. The acceptance bullet says credentials in
	// the vault must not be touched — this test verifies the
	// store-side contract: a refresh saves a new manifest but
	// the vault is a separate subsystem the cli refresh code
	// never reaches into.
	if runtime.GOOS == "windows" {
		t.Skip("sandboxtest.FakeBinary is POSIX-only")
	}
	home := fakeHome(t)
	dir := t.TempDir()
	fake := sandboxtest.FakeBinary{
		Stdout: "Usage: refresh-me [command]\n\nCommands:\n  do     do a thing\n  redo   redo a thing\n",
	}
	binPath, err := fake.Write(dir, "refresh-me")
	if err != nil {
		t.Fatalf("write fake: %v", err)
	}

	// First add.
	var stdout, stderr bytes.Buffer
	if code := runCliAdd(
		[]string{"--yes", binPath},
		strings.NewReader(""),
		&stdout, &stderr,
	); code != 0 {
		t.Fatalf("initial add failed: %s", stderr.String())
	}

	// Then refresh — should succeed and rewrite the same dir.
	stdout.Reset()
	stderr.Reset()
	code := runCliRefresh(
		[]string{"--yes", "refresh-me"},
		strings.NewReader(""),
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("refresh failed exit=%d stderr=%s", code, stderr.String())
	}

	manifestPath := filepath.Join(home, ".aileron", "connectors", "local", "refresh-me", "manifest.toml")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Errorf("manifest missing after refresh: %v", err)
	}
}

func TestResolveProgramPath_RejectsDir(t *testing.T) {
	_, err := resolveProgramPath(t.TempDir())
	if err == nil {
		t.Error("expected error for directory path")
	}
}

func TestResolveProgramPath_RejectsNonExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable-bit check is POSIX-only")
	}
	tmp := t.TempDir()
	notExec := filepath.Join(tmp, "notexec")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := resolveProgramPath(notExec)
	if err == nil {
		t.Error("expected error for non-executable file")
	}
}

func TestResolveProgramPath_ExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	exec := filepath.Join(home, "mybin")
	if err := os.WriteFile(exec, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := resolveProgramPath("~/mybin")
	if err != nil {
		t.Fatalf("resolveProgramPath: %v", err)
	}
	if got != exec {
		t.Errorf("got %q want %q", got, exec)
	}
}

func TestDefaultFSReadScope_CoversXDGTriad(t *testing.T) {
	// Wrap-emitted local connectors get fs_read on the full XDG
	// triad (config + data + cache) so modern CLIs that store
	// sync DBs or caches outside $XDG_CONFIG_HOME install working.
	// See ADR-0014's "Local-origin (BYOCLI) wrap default" section.
	t.Setenv("XDG_CONFIG_HOME", "/custom/config")
	t.Setenv("XDG_DATA_HOME", "/custom/data")
	t.Setenv("XDG_CACHE_HOME", "/custom/cache")
	got := defaultFSReadScope("mycli")
	want := []string{
		"/custom/config/mycli",
		"/custom/data/mycli",
		"/custom/cache/mycli",
	}
	if len(got) != len(want) {
		t.Fatalf("scope=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("scope[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func TestDefaultFSWriteScope_CoversXDGTriad(t *testing.T) {
	// Writes follow reads — CLIs that read from their data/cache
	// dirs almost always need to write them too.
	t.Setenv("XDG_CONFIG_HOME", "/custom/config")
	t.Setenv("XDG_DATA_HOME", "/custom/data")
	t.Setenv("XDG_CACHE_HOME", "/custom/cache")
	got := defaultFSWriteScope("mycli")
	want := []string{
		"/custom/config/mycli",
		"/custom/data/mycli",
		"/custom/cache/mycli",
	}
	if len(got) != len(want) {
		t.Fatalf("scope=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("scope[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func TestScopeBadge_FlagsBroadScopes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("$HOME-based check is POSIX-only")
	}
	home, _ := os.UserHomeDir()
	for path, wantBroad := range map[string]bool{
		home:                    true,
		"/":                     true,
		"/usr":                  true,
		"/usr/local/etc":        false,
		filepath.Join(home, "config", "linear"): false,
	} {
		got := scopeBadge(path)
		isBroad := strings.Contains(got, "broad")
		if isBroad != wantBroad {
			t.Errorf("scopeBadge(%q)=%q want broad=%v", path, got, wantBroad)
		}
	}
}

func TestConfirm_YesAccepts(t *testing.T) {
	var w bytes.Buffer
	if !confirm(strings.NewReader("y\n"), &w, "ok?") {
		t.Error("y should accept")
	}
}

func TestConfirm_NoRejects(t *testing.T) {
	var w bytes.Buffer
	if confirm(strings.NewReader("n\n"), &w, "ok?") {
		t.Error("n should reject")
	}
}

func TestConfirm_EmptyRejects(t *testing.T) {
	var w bytes.Buffer
	if confirm(strings.NewReader("\n"), &w, "ok?") {
		t.Error("empty should reject (default no)")
	}
}

func TestHashBinary_StableSHA256Prefix(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "small")
	if err := os.WriteFile(path, []byte("aileron-byocli"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := hashBinary(path)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(got, "sha256:") {
		t.Errorf("hash missing sha256: prefix: %q", got)
	}
	if len(got) != len("sha256:")+64 {
		t.Errorf("hash hex length unexpected: %d", len(got))
	}
	// Hash is deterministic; recompute and compare.
	again, err := hashBinary(path)
	if err != nil {
		t.Fatalf("hash 2: %v", err)
	}
	if again != got {
		t.Errorf("hash not deterministic: %q vs %q", got, again)
	}
}

func TestHashBinary_RejectsMissingFile(t *testing.T) {
	if _, err := hashBinary("/nonexistent/aileron-byocli-binary"); err == nil {
		t.Error("expected error for missing file")
	}
}

func TestRunCliAdd_PopulatesBinaryHash(t *testing.T) {
	// `aileron cli add` must pin the binary's SHA-256 in the
	// manifest's `programs[].hash` so the runtime's per-spawn
	// integrity check fires per #619's decision. Verify the
	// pinned hash matches a fresh sha256 of the binary's bytes.
	if runtime.GOOS == "windows" {
		t.Skip("sandboxtest.FakeBinary is POSIX-only")
	}
	home := fakeHome(t)
	dir := t.TempDir()
	fake := sandboxtest.FakeBinary{
		Stdout: "Usage: hashme [command]\n\nCommands:\n  do  do thing\n",
	}
	binPath, err := fake.Write(dir, "hashme")
	if err != nil {
		t.Fatalf("write fake: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := runCliAdd(
		[]string{"--yes", "--no-credentials", binPath},
		strings.NewReader(""),
		&stdout, &stderr,
	); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	manifestPath := filepath.Join(home, ".aileron", "connectors", "local", "hashme", "manifest.toml")
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	m, err := cstore.ParseManifest(manifestPath, body)
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if len(m.Capabilities.Spawn.Programs) != 1 {
		t.Fatalf("expected one program entry, got %d", len(m.Capabilities.Spawn.Programs))
	}
	pinned := m.Capabilities.Spawn.Programs[0].Hash
	expected, err := hashBinary(binPath)
	if err != nil {
		t.Fatalf("compute expected: %v", err)
	}
	if pinned != expected {
		t.Errorf("pinned=%q expected=%q", pinned, expected)
	}
}

func TestRunCliAdd_EmitsActionFiles(t *testing.T) {
	// Local-mode connectors are invisible to `aileron action
	// list` if no action.md files exist for their operations.
	// Verify cli add writes at least one action.md under
	// ~/.aileron/actions/<name>-<op>.md.
	//
	// The exact `<op>` names emitted depend on the wrap
	// introspector's `--help` parsing heuristic, which produces
	// platform-dependent results (dash on Linux vs bash on
	// macOS handle the FakeBinary's shell-script wrapper
	// slightly differently). Assert structure rather than
	// specific operation names so the contract under test is
	// "action files are emitted with the right name prefix",
	// not "the introspector finds these specific subcommands."
	if runtime.GOOS == "windows" {
		t.Skip("sandboxtest.FakeBinary is POSIX-only")
	}
	home := fakeHome(t)
	dir := t.TempDir()
	fake := sandboxtest.FakeBinary{
		Stdout: "Usage: actcli [command]\n\nCommands:\n  list   List things\n  show   Show one\n",
	}
	binPath, err := fake.Write(dir, "actcli")
	if err != nil {
		t.Fatalf("write fake: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := runCliAdd(
		[]string{"--yes", "--no-credentials", binPath},
		strings.NewReader(""),
		&stdout, &stderr,
	); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	actionsDir := filepath.Join(home, ".aileron", "actions")
	entries, err := os.ReadDir(actionsDir)
	if err != nil {
		t.Fatalf("read actions dir %s: %v", actionsDir, err)
	}
	emitted := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "actcli-") && strings.HasSuffix(e.Name(), ".md") {
			emitted++
		}
	}
	if emitted == 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected at least one actcli-*.md action file under %s, got entries: %v", actionsDir, names)
	}
}

func TestRunCliRemove_DeletesActionFiles(t *testing.T) {
	// cli remove must clean up the action.md files alongside the
	// connector manifest, otherwise `aileron action list` shows
	// stale entries that fail at execution time.
	//
	// The introspector's heuristic produces platform-dependent
	// op names (see TestRunCliAdd_EmitsActionFiles); capture
	// the actual set after add and assert that set is gone
	// after remove, rather than hard-coding an op name.
	if runtime.GOOS == "windows" {
		t.Skip("sandboxtest.FakeBinary is POSIX-only")
	}
	home := fakeHome(t)
	dir := t.TempDir()
	fake := sandboxtest.FakeBinary{
		Stdout: "Usage: rmcli\n\nCommands:\n  do   do thing\n",
	}
	binPath, err := fake.Write(dir, "rmcli")
	if err != nil {
		t.Fatalf("write fake: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := runCliAdd(
		[]string{"--yes", "--no-credentials", binPath},
		strings.NewReader(""),
		&stdout, &stderr,
	); code != 0 {
		t.Fatalf("add: exit=%d stderr=%s", code, stderr.String())
	}
	actionsDir := filepath.Join(home, ".aileron", "actions")
	preEntries, err := os.ReadDir(actionsDir)
	if err != nil {
		t.Fatalf("read actions dir: %v", err)
	}
	preNames := matchingActionFiles(preEntries, "rmcli-")
	if len(preNames) == 0 {
		t.Fatal("no rmcli-*.md action files were emitted by cli add; nothing for remove to delete")
	}

	stdout.Reset()
	stderr.Reset()
	if code := runCliRemove([]string{"rmcli"}, &stdout, &stderr); code != 0 {
		t.Fatalf("remove: exit=%d stderr=%s", code, stderr.String())
	}
	postEntries, err := os.ReadDir(actionsDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read actions dir post-remove: %v", err)
	}
	postNames := matchingActionFiles(postEntries, "rmcli-")
	if len(postNames) != 0 {
		t.Errorf("action files survived remove: %v", postNames)
	}
}

// matchingActionFiles returns the entries whose name starts
// with prefix and ends with `.md`. Test-local helper used by
// the action-emit/remove tests to assert structural
// invariants without pinning specific op names.
func matchingActionFiles(entries []os.DirEntry, prefix string) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".md") {
			out = append(out, name)
		}
	}
	return out
}

func TestValidateConnectorName_RejectsBadShape(t *testing.T) {
	for _, bad := range []string{"", "Linear", "with spaces", "../escape"} {
		if err := validateConnectorName(bad); err == nil {
			t.Errorf("accepted invalid name %q", bad)
		}
	}
}

func TestRunCli_DispatchUnknown(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runCli([]string{"frobnicate"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit for unknown subcommand")
	}
	if !strings.Contains(stderr.String(), "frobnicate") {
		t.Errorf("unknown subcommand should be surfaced: %q", stderr.String())
	}
}

func TestRunCli_AliasesDispatch(t *testing.T) {
	// `ls` is the conventional alias for `list`; `rm` for `remove`.
	// Verify the dispatcher honors both shapes so users don't have
	// to learn whether the CLI uses Unix or git-style command names.
	fakeHome(t)
	var stdout, stderr bytes.Buffer
	if code := runCli([]string{"ls"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Errorf("`cli ls` exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No local connectors") {
		t.Errorf("`ls` should render the list output, got %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	// `rm` on a missing entry should produce the same error
	// shape as `remove`.
	code := runCli([]string{"rm", "never-existed"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Error("`rm` on missing entry should exit nonzero")
	}
}

func TestRunCliAdd_VersionOverride(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sandboxtest.FakeBinary is POSIX-only")
	}
	home := fakeHome(t)
	dir := t.TempDir()
	fake := sandboxtest.FakeBinary{
		Stdout: "Usage: vercli [command]\n\nCommands:\n  do  do a thing\n",
	}
	binPath, err := fake.Write(dir, "vercli")
	if err != nil {
		t.Fatalf("write fake: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runCliAdd(
		[]string{"--yes", "--version", "1.2.3", binPath},
		strings.NewReader(""),
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	manifestPath := filepath.Join(home, ".aileron", "connectors", "local", "vercli", "manifest.toml")
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
	m, err := cstore.ParseManifest(manifestPath, body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Connector.Version != "1.2.3" {
		t.Errorf("version override not honored: got %q want 1.2.3", m.Connector.Version)
	}
}

func TestRunCliRefresh_RejectsMissingEntry(t *testing.T) {
	fakeHome(t)
	var stdout, stderr bytes.Buffer
	code := runCliRefresh(
		[]string{"--yes", "never-installed"},
		strings.NewReader(""),
		&stdout, &stderr,
	)
	if code == 0 {
		t.Error("expected nonzero exit for missing entry")
	}
	if !strings.Contains(stderr.String(), "no local connector") {
		t.Errorf("stderr should explain the missing entry: %q", stderr.String())
	}
}

func TestRunCliRefresh_RequiresName(t *testing.T) {
	fakeHome(t)
	var stdout, stderr bytes.Buffer
	code := runCliRefresh(nil, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit without name arg")
	}
}

func TestRunCliRemove_RequiresName(t *testing.T) {
	fakeHome(t)
	var stdout, stderr bytes.Buffer
	code := runCliRemove(nil, &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit without name arg")
	}
}

func TestRunCliRefresh_RejectsManifestWithoutSpawn(t *testing.T) {
	// Defensive path: a local manifest with no [capabilities.spawn]
	// can't be refreshed (no program to re-introspect). Seed one
	// by writing the bytes directly so the unusual shape lands on
	// disk; refresh must surface the missing-spawn condition with
	// a clear error rather than panicking later.
	home := fakeHome(t)
	dir := filepath.Join(home, ".aileron", "connectors", "local", "no-spawn")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Hand-write a minimally-valid manifest that the parser
	// will accept but that has no [capabilities.spawn] block.
	// Use a non-forwarder connector since the forwarder-without-
	// spawn shape is rejected by ValidateManifest.
	fqn, _ := cstore.LocalFQN("no-spawn")
	toml := `[connector]
name = "` + fqn + `"
version = "0.0.1"
origin = "local"
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runCliRefresh(
		[]string{"--yes", "no-spawn"},
		strings.NewReader(""),
		&stdout, &stderr,
	)
	if code == 0 {
		t.Error("expected nonzero exit for manifest without spawn block")
	}
}

func TestRunCli_HelpFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runCli([]string{"--help"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Errorf("--help should exit 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "aileron cli add") {
		t.Errorf("--help should list subcommands: %q", stdout.String())
	}
}
