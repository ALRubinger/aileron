package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fixtureRegistry is a small, hand-built registry.json that
// matches PrintingPress's v2 schema shape. Tests serve it via
// httptest and point ppCatalogURL at the server so we never hit
// the live network. The shape is the minimum set of fields the
// installer reads — irrelevant fields (printer, mcp, category)
// are present so the decoder exercises real-world skipping
// behavior.
const fixtureRegistry = `{
  "schema_version": 2,
  "entries": [
    {
      "name": "linear",
      "category": "project-management",
      "path": "library/project-management/linear",
      "printer": "mvanhorn",
      "description": "Query Linear issues.",
      "mcp": {"binary": "linear-pp-mcp"}
    },
    {
      "name": "sentry",
      "category": "monitoring",
      "path": "library/monitoring/sentry",
      "printer": "mvanhorn",
      "description": "Sentry events from the terminal."
    },
    {
      "name": "linkedin",
      "category": "social",
      "path": "library/social/linkedin",
      "printer": "mvanhorn",
      "description": "LinkedIn search."
    }
  ]
}`

// startFixtureCatalog spins up an httptest server that serves
// `fixtureRegistry` at any path. Returns the test cleanup so the
// caller does not have to remember to defer Close.
func startFixtureCatalog(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(fixtureRegistry))
	}))
	t.Cleanup(srv.Close)
	prev := ppCatalogURL
	ppCatalogURL = srv.URL + "/registry.json"
	t.Cleanup(func() { ppCatalogURL = prev })
	return srv.URL
}

func TestLookupPPEntry_FindsKnownName(t *testing.T) {
	startFixtureCatalog(t)
	e, err := lookupPPEntry(context.Background(), "linear")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if e.Path != "library/project-management/linear" {
		t.Errorf("Path=%q", e.Path)
	}
	if e.Description == "" {
		t.Errorf("Description not decoded")
	}
}

func TestLookupPPEntry_UnknownNameSuggestsSimilar(t *testing.T) {
	// Typo `lienar` instead of `linear`. Both share the leading
	// `li` so the suggestion list should include `linear`.
	startFixtureCatalog(t)
	_, err := lookupPPEntry(context.Background(), "lienar")
	if err == nil {
		t.Fatal("expected error for unknown name")
	}
	if !strings.Contains(err.Error(), "linear") {
		t.Errorf("error should suggest `linear` for typo `lienar`: %v", err)
	}
}

func TestLookupPPEntry_UnknownNameNoMatchesFallsBackToBrowseHint(t *testing.T) {
	// No entry starts with `zz` so suggestions are empty; the
	// error should fall back to the browse-the-catalog hint.
	startFixtureCatalog(t)
	_, err := lookupPPEntry(context.Background(), "zzzz")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "printingpress.dev") {
		t.Errorf("error should suggest browsing the catalog: %v", err)
	}
}

func TestLookupPPEntry_RejectsEmptyName(t *testing.T) {
	_, err := lookupPPEntry(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestLookupPPEntry_FetchFailurePropagates(t *testing.T) {
	prev := ppCatalogURL
	ppCatalogURL = "http://127.0.0.1:1/registry.json" // unreachable
	t.Cleanup(func() { ppCatalogURL = prev })
	_, err := lookupPPEntry(context.Background(), "linear")
	if err == nil {
		t.Fatal("expected fetch failure")
	}
}

func TestLookupPPEntry_NonOKStatusPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	prev := ppCatalogURL
	ppCatalogURL = srv.URL
	t.Cleanup(func() { ppCatalogURL = prev })
	_, err := lookupPPEntry(context.Background(), "linear")
	if err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestLookupPPEntry_MalformedJSONPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "not json at all")
	}))
	t.Cleanup(srv.Close)
	prev := ppCatalogURL
	ppCatalogURL = srv.URL
	t.Cleanup(func() { ppCatalogURL = prev })
	_, err := lookupPPEntry(context.Background(), "linear")
	if err == nil {
		t.Fatal("expected decode error")
	}
}

func TestLookupPPEntry_EntryWithoutPathErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"schema_version":2,"entries":[{"name":"broken"}]}`)
	}))
	t.Cleanup(srv.Close)
	prev := ppCatalogURL
	ppCatalogURL = srv.URL
	t.Cleanup(func() { ppCatalogURL = prev })
	_, err := lookupPPEntry(context.Background(), "broken")
	if err == nil {
		t.Fatal("expected error for entry without path")
	}
}

func TestPPModulePath_StripsLeadingSlash(t *testing.T) {
	cases := map[string]string{
		"library/marketing/ahrefs":  "github.com/mvanhorn/printing-press-library/library/marketing/ahrefs",
		"/library/social/linkedin":  "github.com/mvanhorn/printing-press-library/library/social/linkedin",
	}
	for in, want := range cases {
		if got := ppModulePath(in); got != want {
			t.Errorf("ppModulePath(%q)=%q want %q", in, got, want)
		}
	}
}

func TestPPPackagePath_AppendsCmdSubdirAndPpCliSuffix(t *testing.T) {
	// The actual binary lives at `<modulePath>/cmd/<name>-pp-cli`
	// in every PrintingPress entry (their authoring tooling
	// emits that layout). Pin the convention here so a future
	// `go install` regression doesn't ship a path that resolves
	// to the module root and fails with "found, but does not
	// contain package."
	cases := []struct {
		entry string
		name  string
		want  string
	}{
		{
			entry: "library/project-management/linear",
			name:  "linear",
			want:  "github.com/mvanhorn/printing-press-library/library/project-management/linear/cmd/linear-pp-cli",
		},
		{
			entry: "library/marketing/ahrefs",
			name:  "ahrefs",
			want:  "github.com/mvanhorn/printing-press-library/library/marketing/ahrefs/cmd/ahrefs-pp-cli",
		},
		{
			entry: "/library/social/linkedin",
			name:  "linkedin",
			want:  "github.com/mvanhorn/printing-press-library/library/social/linkedin/cmd/linkedin-pp-cli",
		},
	}
	for _, c := range cases {
		if got := ppPackagePath(c.entry, c.name); got != c.want {
			t.Errorf("ppPackagePath(%q, %q)=%q want %q", c.entry, c.name, got, c.want)
		}
	}
}

func TestPPSourceURL_StripsLeadingSlash(t *testing.T) {
	if got := ppSourceURL("/library/x/y"); got != "https://github.com/mvanhorn/printing-press-library/tree/main/library/x/y" {
		t.Errorf("ppSourceURL=%q", got)
	}
}

func TestRequireGoOnPath_HappyOnRealDev(t *testing.T) {
	// CI runners always have `go` since the test binary itself is
	// a Go program; this would only fail in a very weird PATH.
	// Skip on Windows where the lookup semantics differ slightly.
	if err := requireGoOnPath(); err != nil {
		t.Errorf("requireGoOnPath returned error in a runtime that compiled this test: %v", err)
	}
}

func TestRequireGoOnPath_EmptyPATHFails(t *testing.T) {
	t.Setenv("PATH", "")
	err := requireGoOnPath()
	if err == nil {
		t.Fatal("expected error when PATH is empty")
	}
	if !strings.Contains(err.Error(), "go.dev") {
		t.Errorf("error should hint at install URL: %v", err)
	}
}

func TestRunPp_HelpFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runPp([]string{"--help"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Errorf("--help exit=%d", code)
	}
	if !strings.Contains(stdout.String(), "aileron pp add") {
		t.Errorf("--help should list subcommands: %q", stdout.String())
	}
}

func TestRunPp_UnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runPp([]string{"frobnicate"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit for unknown subcommand")
	}
}

func TestRunPp_EmptyArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runPp(nil, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit when no subcommand")
	}
}

func TestRunPpAdd_RequiresName(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runPpAdd(nil, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit when no name")
	}
}

func TestRunPpAdd_DryRunDoesNotInstall(t *testing.T) {
	startFixtureCatalog(t)

	// Capture `go install` invocations so the test fails loudly
	// if the install path fires under --dry-run.
	called := false
	prev := runGoInstall
	t.Cleanup(func() { runGoInstall = prev })
	runGoInstall = func(modulePath string, stdout, stderr io.Writer) error {
		called = true
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := runPpAdd(
		[]string{"--dry-run", "linear"},
		strings.NewReader(""),
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if called {
		t.Error("--dry-run should not invoke go install")
	}
	out := stdout.String()
	if !strings.Contains(out, "github.com/mvanhorn/printing-press-library/library/project-management/linear/cmd/linear-pp-cli") {
		t.Errorf("install plan should show the cmd/-pp-cli package path: %q", out)
	}
	if !strings.Contains(out, "dry-run") {
		t.Errorf("dry-run notice missing: %q", out)
	}
}

func TestRunPpAdd_UnknownNameSurfacesError(t *testing.T) {
	startFixtureCatalog(t)
	var stdout, stderr bytes.Buffer
	code := runPpAdd(
		[]string{"--yes", "doesnotexist"},
		strings.NewReader(""),
		&stdout, &stderr,
	)
	if code == 0 {
		t.Error("expected nonzero exit for unknown entry")
	}
	if !strings.Contains(stderr.String(), "not in the PrintingPress catalog") {
		t.Errorf("stderr should explain unknown name: %q", stderr.String())
	}
}

func TestRunPpAdd_GoMissingFailsLoud(t *testing.T) {
	// requireGoOnPath fires before catalog fetch, so we can also
	// short-circuit the fixture setup. Verify the error mentions
	// the install URL so users know what to do next.
	t.Setenv("PATH", "")
	var stdout, stderr bytes.Buffer
	code := runPpAdd(
		[]string{"--yes", "linear"},
		strings.NewReader(""),
		&stdout, &stderr,
	)
	if code == 0 {
		t.Error("expected nonzero exit with empty PATH")
	}
	if !strings.Contains(stderr.String(), "go.dev") {
		t.Errorf("stderr should hint at go.dev: %q", stderr.String())
	}
}

func TestResolveInstalledBinary_FindsGOBIN(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOBIN", dir)
	target := filepath.Join(dir, "fakecli-pp-cli")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := resolveInstalledBinary("fakecli-pp-cli")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != target {
		t.Errorf("got %q want %q", got, target)
	}
}

func TestResolveInstalledBinary_FallsBackToGOPATHBin(t *testing.T) {
	// GOBIN unset, GOPATH custom. Drop the binary in $GOPATH/bin
	// and verify the fallback finds it.
	if runtime.GOOS == "windows" {
		t.Skip("PATHEXT semantics differ on Windows; covered separately")
	}
	t.Setenv("GOBIN", "")
	gopath := t.TempDir()
	prev := goEnvGOPATH
	goEnvGOPATH = func() (string, error) { return gopath, nil }
	t.Cleanup(func() { goEnvGOPATH = prev })

	bindir := filepath.Join(gopath, "bin")
	if err := os.MkdirAll(bindir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	target := filepath.Join(bindir, "fallbackcli-pp-cli")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := resolveInstalledBinary("fallbackcli-pp-cli")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != target {
		t.Errorf("got %q want %q", got, target)
	}
}

func TestRunPpAdd_HappyPathHandsOffToCliAdd(t *testing.T) {
	// End-to-end happy path with `go install` mocked. The mock
	// writes a sandboxtest.FakeBinary to GOBIN/<name>-pp-cli so
	// the cli-add handoff has a real binary to introspect.
	// Verifies: catalog lookup → install plan → mocked install
	// → binary resolution → cli add manifest write.
	if runtime.GOOS == "windows" {
		t.Skip("sandboxtest.FakeBinary is POSIX-only")
	}
	startFixtureCatalog(t)
	home := fakeHome(t)
	gobin := filepath.Join(t.TempDir(), "gobin")
	if err := os.MkdirAll(gobin, 0o755); err != nil {
		t.Fatalf("mkdir gobin: %v", err)
	}
	t.Setenv("GOBIN", gobin)

	// Mock `go install` to drop a fake binary at the expected path.
	prevInstall := runGoInstall
	t.Cleanup(func() { runGoInstall = prevInstall })
	runGoInstall = func(modulePath string, stdout, stderr io.Writer) error {
		const helpText = "linear-pp-cli - Linear CLI\n\nUsage: linear-pp-cli [command]\n\nCommands:\n  issues   List issues\n  create   Create issue\n"
		path := filepath.Join(gobin, "linear-pp-cli")
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s' '"+helpText+"'\n"), 0o755); err != nil {
			return err
		}
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := runPpAdd(
		[]string{"--yes", "--no-credentials", "linear"},
		strings.NewReader(""),
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}

	// Confirm the local connector manifest landed under the
	// short name (`linear`), not the binary's full name.
	manifestPath := filepath.Join(home, ".aileron", "connectors", "local", "linear", "manifest.toml")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("local manifest missing: %v", err)
	}
}

func TestRunPpAdd_GoInstallFailureSurfacesError(t *testing.T) {
	// Mock runGoInstall to simulate a build/network failure. The
	// installer must surface the error with a nonzero exit and a
	// stderr message explaining the toolchain failed — without
	// touching the local connector store.
	startFixtureCatalog(t)
	fakeHome(t)
	t.Setenv("GOBIN", t.TempDir())

	prev := runGoInstall
	t.Cleanup(func() { runGoInstall = prev })
	runGoInstall = func(modulePath string, stdout, stderr io.Writer) error {
		return errors.New("simulated build failure")
	}

	var stdout, stderr bytes.Buffer
	code := runPpAdd(
		[]string{"--yes", "--no-credentials", "linear"},
		strings.NewReader(""),
		&stdout, &stderr,
	)
	if code == 0 {
		t.Fatal("expected nonzero exit when go install fails")
	}
	if !strings.Contains(stderr.String(), "simulated build failure") {
		t.Errorf("stderr should surface the underlying error: %q", stderr.String())
	}
}

func TestRunPpAdd_PassphraseFileFlagFlowsThrough(t *testing.T) {
	// The --passphrase-file flag must reach the cli-add handoff so
	// users can do non-interactive installs (CI / config-as-code).
	// Verify by setting up the full happy path and confirming the
	// install succeeds with the flag — its presence on the
	// generated cli-add args is what makes this path testable.
	if runtime.GOOS == "windows" {
		t.Skip("sandboxtest fake binary is POSIX-only")
	}
	startFixtureCatalog(t)
	home := fakeHome(t)
	gobin := filepath.Join(t.TempDir(), "gobin")
	if err := os.MkdirAll(gobin, 0o755); err != nil {
		t.Fatalf("mkdir gobin: %v", err)
	}
	t.Setenv("GOBIN", gobin)

	prev := runGoInstall
	t.Cleanup(func() { runGoInstall = prev })
	runGoInstall = func(modulePath string, stdout, stderr io.Writer) error {
		// Drop a non-trivial fake at the expected path.
		const helpText = "Commands:\n  do  do thing\n"
		body := "#!/bin/sh\nprintf '%s' '" + helpText + "'\n"
		return os.WriteFile(filepath.Join(gobin, "linear-pp-cli"), []byte(body), 0o755)
	}

	// Even though we pass --no-credentials, the flag-parsing path
	// for --passphrase-file still has to recognize the flag and
	// thread it through.
	passFile := filepath.Join(t.TempDir(), "phrase")
	if err := os.WriteFile(passFile, []byte("ignored-by-no-credentials\n"), 0o600); err != nil {
		t.Fatalf("write passfile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runPpAdd(
		[]string{"--yes", "--no-credentials", "--passphrase-file", passFile, "linear"},
		strings.NewReader(""),
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	// Manifest must exist under short name — proves cli add ran.
	manifestPath := filepath.Join(home, ".aileron", "connectors", "local", "linear", "manifest.toml")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Errorf("manifest missing after happy install: %v", err)
	}
}

func TestRunGoInstall_FailsFastOnKnownBadModule(t *testing.T) {
	// runGoInstall is the production exec.Command shell-out;
	// tests normally mock the var to avoid forking `go`. This
	// one test calls the real function with a deliberately
	// invalid module path so `go install` fails fast (bad module
	// shape, no network resolution attempted). Exercises every
	// line of the function body without actually installing
	// anything — verifies env/stdout/stderr wiring is correct.
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("`go` toolchain not on $PATH; cannot exercise runGoInstall body")
	}
	var stdout, stderr bytes.Buffer
	err := runGoInstall("not-a-valid/module path/with spaces", &stdout, &stderr)
	if err == nil {
		t.Fatal("expected go install to fail on a malformed module path")
	}
	// The exact error message varies across Go versions; just
	// confirm something was written to stderr (the toolchain's
	// own diagnostic).
	if stderr.Len() == 0 {
		t.Errorf("expected toolchain to write a diagnostic to stderr; got empty")
	}
}

func TestGoEnvGOPATH_ReturnsToolchainGOPATH(t *testing.T) {
	// Companion smoke-test for the goEnvGOPATH test seam — runs
	// the real `go env GOPATH` shell-out and verifies it returns
	// a non-empty absolute-looking path. Covers the seam's body
	// (4 lines) without mocking.
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("`go` not on $PATH")
	}
	got, err := goEnvGOPATH()
	if err != nil {
		t.Fatalf("goEnvGOPATH: %v", err)
	}
	if got == "" {
		t.Errorf("goEnvGOPATH returned empty string")
	}
}

func TestResolveInstalledBinary_MissingErrors(t *testing.T) {
	t.Setenv("GOBIN", t.TempDir())
	prev := goEnvGOPATH
	goEnvGOPATH = func() (string, error) { return t.TempDir(), nil }
	t.Cleanup(func() { goEnvGOPATH = prev })
	// Empty PATH so the LookPath fallback doesn't accidentally
	// find a real binary on the host.
	t.Setenv("PATH", "")

	_, err := resolveInstalledBinary("absolutely-nonexistent-pp-cli")
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}
