//go:build darwin

package sandbox

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Each test below cites the ADR-0014 clause or the macOS SBPL
// property it enforces so refactors that preserve the contract
// leave the test green.

func TestApplyPlatformSandbox_Darwin_NoOpWhenNoParamsDeclared(t *testing.T) {
	// Contract (ADR-0014 graceful unavailability + legacy compat):
	// when the manifest declares nothing platform-sandbox-relevant
	// the function returns nil and leaves cmd untouched. This
	// preserves the executor's pre-sandbox behavior for tests that
	// don't exercise the sandbox.
	cmd := exec.CommandContext(context.Background(), "/bin/echo", "hello")
	origPath := cmd.Path
	origArgs := append([]string(nil), cmd.Args...)

	if _, err := applyPlatformSandbox(cmd, SpawnEnvelope{Program: "/bin/echo"}, SpawnLimits{}); err != nil {
		t.Fatalf("expected nil with empty limits, got %v", err)
	}
	if cmd.Path != origPath {
		t.Errorf("cmd.Path mutated: %q -> %q", origPath, cmd.Path)
	}
	if !sliceEq(cmd.Args, origArgs) {
		t.Errorf("cmd.Args mutated: %v -> %v", origArgs, cmd.Args)
	}
}

func TestApplyPlatformSandbox_Darwin_WrapsCmdWhenParamsDeclared(t *testing.T) {
	// Contract: when the manifest declares scopes, cmd is rewired
	// so the kernel exec's sandbox-exec, which applies the profile
	// and exec's the wrapped program.
	cmd := exec.CommandContext(context.Background(), "/bin/echo", "hello")
	limits := SpawnLimits{FSRead: []string{"~/code/"}}
	if _, err := applyPlatformSandbox(cmd, SpawnEnvelope{Program: "/bin/echo"}, limits); err != nil {
		t.Fatalf("applyPlatformSandbox: %v", err)
	}
	if cmd.Path != sandboxExecPath {
		t.Errorf("cmd.Path = %q, want %q", cmd.Path, sandboxExecPath)
	}
	if len(cmd.Args) < 4 {
		t.Fatalf("cmd.Args too short: %v", cmd.Args)
	}
	if cmd.Args[0] != "sandbox-exec" {
		t.Errorf("argv[0] = %q, want sandbox-exec", cmd.Args[0])
	}
	if cmd.Args[1] != "-p" {
		t.Errorf("argv[1] = %q, want -p", cmd.Args[1])
	}
	// Profile is argv[2]; wrapped program is argv[3]; wrapped argv
	// tail is argv[4..]
	if cmd.Args[3] != "/bin/echo" {
		t.Errorf("wrapped program = %q, want /bin/echo", cmd.Args[3])
	}
	if cmd.Args[4] != "hello" {
		t.Errorf("wrapped arg = %q, want hello", cmd.Args[4])
	}
}

func TestBuildSBPLProfile_HasPermissiveBaselineWithUserDenies(t *testing.T) {
	// Contract: per the BYOCLI threat model in ADR-0014's macOS
	// section, the profile uses an `(allow default)` baseline plus
	// surgical denies on user-private paths and writes. The wrapped
	// program name appears as a trailing comment for post-hoc
	// inspection.
	env := SpawnEnvelope{Program: "/usr/bin/git"}
	profile := buildSBPLProfile(env, SpawnLimits{FSRead: []string{"~/code/"}})
	mustContain(t, profile,
		"(version 1)",
		"(allow default)",
		"(deny file-write*)",
		`(deny file-read* (subpath "/Users"))`,
		`(deny file-read* (subpath "/Volumes"))`,
		"(deny network*)",
		"; wrapped program: /usr/bin/git",
	)
}

func TestBuildSBPLProfile_AllowsReadOnSpawnedBinaryDir(t *testing.T) {
	// Contract: the profile auto-allows file-read* on the spawned
	// binary's parent directory and that allow MUST appear after
	// the /Users deny so the SBPL evaluator reinstates it. Without
	// this, macOS Security framework's `SecPolicyCreateSSL` fails
	// for any Go binary installed under /Users (the default
	// `aileron pp add` layout), breaking every HTTPS call.
	//
	// `subpath` is required: a `literal` rule on the binary file
	// alone does not satisfy Security framework, which also stats
	// the surrounding directory while determining code-signing
	// identity for keychain access scoping.
	env := SpawnEnvelope{Program: "/Users/someone/.aileron/connectors/local/foo/bin/foo-cli"}
	profile := buildSBPLProfile(env, SpawnLimits{FSRead: []string{"~/code/"}})
	wantRule := `(allow file-read* (subpath "/Users/someone/.aileron/connectors/local/foo/bin"))`
	if !strings.Contains(profile, wantRule) {
		t.Errorf("profile missing binary-dir allow rule %q:\n%s", wantRule, profile)
	}
	// Rule ordering: the auto-allow must follow the /Users deny.
	idxDeny := strings.Index(profile, `(deny file-read* (subpath "/Users"))`)
	idxAllow := strings.Index(profile, wantRule)
	if idxDeny < 0 || idxAllow < 0 || idxAllow < idxDeny {
		t.Errorf("binary-dir allow must follow /Users deny (deny=%d allow=%d):\n%s",
			idxDeny, idxAllow, profile)
	}
}

func TestBuildSBPLProfile_BinaryDirAllowExpandsTilde(t *testing.T) {
	// Contract: when env.Program is ~/-anchored, the auto-allow
	// rule emits the expanded absolute path so SBPL (which has no
	// shell-style expansion) can match it at runtime.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	env := SpawnEnvelope{Program: "~/bin/my-cli"}
	profile := buildSBPLProfile(env, SpawnLimits{})
	wantRule := `(allow file-read* (subpath "` + filepath.Join(home, "bin") + `"))`
	if !strings.Contains(profile, wantRule) {
		t.Errorf("profile missing expanded binary-dir allow %q:\n%s", wantRule, profile)
	}
}

func TestBuildSBPLProfile_TranslatesFSScopes(t *testing.T) {
	// Contract: trailing-slash manifest paths become subpath rules;
	// slashless paths become literal rules. Tilde expands to the
	// home directory at profile-generation time. Manifest scopes
	// appear AFTER the user-path denies so the SBPL evaluator
	// reinstates allow for the specific subtree.
	limits := SpawnLimits{
		FSRead:  []string{"~/code/", "/etc/passwd", "/private/etc/", "/usr/local/share/data.json"},
		FSWrite: []string{"~/.cache/aileron/"},
	}
	profile := buildSBPLProfile(SpawnEnvelope{Program: "/usr/bin/git"}, limits)
	mustContain(t, profile,
		`(allow file-read* (literal "/etc/passwd"))`,
		`(allow file-read* (subpath "/private/etc"))`,
		`(allow file-read* (literal "/usr/local/share/data.json"))`,
	)
	if !strings.Contains(profile, "file-write*") {
		t.Errorf("profile missing file-write* rule: %s", profile)
	}

	// Manifest read scopes must appear after the /Users deny so the
	// rule ordering reinstates the allow for the specific subtree.
	idxDeny := strings.Index(profile, `(deny file-read* (subpath "/Users"))`)
	// We don't know the user's home directory in this test
	// environment, so we just check that *some* manifest-scope
	// re-allow appears after the /Users deny.
	idxAllow := strings.Index(profile[idxDeny:], `(allow file-read* `)
	if idxAllow < 0 {
		t.Errorf("expected at least one (allow file-read* ...) after the /Users deny:\n%s", profile)
	}
}

func TestBuildSBPLProfile_NetworkDeniedByDefaultButProxyAllowed(t *testing.T) {
	// Contract (ADR-0014 network confinement): without a proxy
	// address, network is denied wholesale; with one, exactly one
	// allow rule names the proxy endpoint.
	//
	// Note: the allow rule MUST emit `localhost`, not `127.0.0.1`,
	// because SBPL's `remote tcp` grammar accepts only `localhost`
	// or `*` for the host portion. An IP literal makes sandbox-exec
	// fail parse with "host must be * or localhost in network
	// address" — the bug fixed in this commit, surfaced once
	// permissive-mode local-origin connectors started running the
	// audit proxy.
	env := SpawnEnvelope{Program: "/usr/bin/git"}

	noNet := buildSBPLProfile(env, SpawnLimits{FSRead: []string{"~/code/"}})
	if !strings.Contains(noNet, "(deny network*)") {
		t.Errorf("no-network profile missing deny network*: %s", noNet)
	}
	if strings.Contains(noNet, "(allow network*") {
		t.Errorf("no-network profile unexpectedly allows network: %s", noNet)
	}

	withNet := buildSBPLProfile(env, SpawnLimits{
		FSRead:    []string{"~/code/"},
		ProxyAddr: "127.0.0.1:54321",
	})
	mustContain(t, withNet,
		"(deny network*)",
		`(allow network* (remote tcp "localhost:54321"))`,
	)
	// Defense in depth: the IP literal must not appear in the
	// emitted rule at all — sandbox-exec parses the file before
	// invoking the wrapped binary, and any rule containing
	// `127.0.0.1` would fail parse and refuse to spawn anything.
	if strings.Contains(withNet, `"127.0.0.1:54321"`) {
		t.Errorf("profile still contains IP literal in SBPL rule (should be `localhost:` form):\n%s", withNet)
	}
}

func TestSBPLProxyHost_RewritesLoopbackToLocalhost(t *testing.T) {
	// The SBPL `remote tcp` host slot accepts only `localhost` or
	// `*` (see Apple's sandbox(7)). The proxy itself binds on
	// `127.0.0.1:<port>` because Go's net package and HTTPS_PROXY
	// both accept the IP literal; only the SBPL rule needs the
	// `localhost` spelling. sbplProxyHost performs that one
	// translation and passes everything else through.
	cases := map[string]string{
		"127.0.0.1:54321":  "localhost:54321",
		"127.0.0.1:8080":   "localhost:8080",
		"localhost:9999":   "localhost:9999",   // already valid SBPL
		"example.com:443":  "example.com:443",  // non-loopback passes through; will fail SBPL parse cleanly
		"127.0.0.1":        "127.0.0.1",        // no port suffix → not a port-form match
	}
	for in, want := range cases {
		got := sbplProxyHost(in)
		if got != want {
			t.Errorf("sbplProxyHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestApplyPlatformSandbox_Darwin_RunsRealBinaryUnderSandbox(t *testing.T) {
	// End-to-end smoke: /bin/echo runs under the generated profile,
	// the sandbox permits the system essentials, the wrapped binary
	// exits 0 and produces its expected output. Asserts that the
	// system-essentials read set is wide enough for at least a
	// trivial Mach-O binary.
	cmd := exec.CommandContext(context.Background(), "/bin/echo", "sandboxed")
	limits := SpawnLimits{FSRead: []string{"~/code/"}}
	if _, err := applyPlatformSandbox(cmd, SpawnEnvelope{Program: "/bin/echo"}, limits); err != nil {
		t.Fatalf("applyPlatformSandbox: %v", err)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("sandboxed /bin/echo failed: %v (cmd=%v)", err, cmd.Args)
	}
	if !strings.Contains(string(out), "sandboxed") {
		t.Errorf("output = %q, want to contain 'sandboxed'", string(out))
	}
}

func TestApplyPlatformSandbox_Darwin_BlocksUserHomeReadOutsideScope(t *testing.T) {
	// Contract (BYOCLI threat model): a binary spawned under a
	// profile whose fs_read does not cover a path under /Users
	// cannot read that path. The kernel sandbox kext rejects the
	// open, the binary errors. This is the load-bearing defense
	// against user-secret exfiltration. /bin/cat is a small Mach-O
	// to use as a probe.
	//
	// Write a sentinel file outside the manifest's scope, then
	// declare an fs_read that does not cover it, then run cat on
	// it and expect failure.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	allowedDir := filepath.Join(home, "aileron-sandbox-test-allowed")
	deniedDir := filepath.Join(home, "aileron-sandbox-test-denied")
	if err := os.MkdirAll(allowedDir, 0o755); err != nil {
		t.Fatalf("mkdir allowed: %v", err)
	}
	if err := os.MkdirAll(deniedDir, 0o755); err != nil {
		t.Fatalf("mkdir denied: %v", err)
	}
	defer os.RemoveAll(allowedDir)
	defer os.RemoveAll(deniedDir)
	sentinel := filepath.Join(deniedDir, "secret.txt")
	if err := os.WriteFile(sentinel, []byte("don't read me"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "/bin/cat", sentinel)
	limits := SpawnLimits{FSRead: []string{allowedDir + "/"}}
	if _, err := applyPlatformSandbox(cmd, SpawnEnvelope{Program: "/bin/cat"}, limits); err != nil {
		t.Fatalf("applyPlatformSandbox: %v", err)
	}
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Fatalf("expected /bin/cat to fail reading %s under sandbox; succeeded with output %q",
			sentinel, string(out))
	}
	// On macOS the sandbox kext makes the read fail with EPERM,
	// which cat reports and exits non-zero. The exact exit code
	// and stderr shape are not part of the contract, only the
	// non-zero exit.
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		t.Fatalf("expected exec.ExitError, got %v", runErr)
	}
	if exitErr.ExitCode() == 0 {
		t.Errorf("expected non-zero exit, got %d", exitErr.ExitCode())
	}
}

func TestApplyPlatformSandbox_Darwin_AllowsUserHomeReadInsideScope(t *testing.T) {
	// Contract (complement of the above): a path under /Users that
	// IS within the declared fs_read scope is readable. The kernel
	// permits the open and cat succeeds.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	allowedDir := filepath.Join(home, "aileron-sandbox-test-allowed-read")
	if err := os.MkdirAll(allowedDir, 0o755); err != nil {
		t.Fatalf("mkdir allowed: %v", err)
	}
	defer os.RemoveAll(allowedDir)
	sentinel := filepath.Join(allowedDir, "data.txt")
	if err := os.WriteFile(sentinel, []byte("hello-from-sandbox"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "/bin/cat", sentinel)
	limits := SpawnLimits{FSRead: []string{allowedDir + "/"}}
	if _, err := applyPlatformSandbox(cmd, SpawnEnvelope{Program: "/bin/cat"}, limits); err != nil {
		t.Fatalf("applyPlatformSandbox: %v", err)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("sandboxed cat failed inside declared scope: %v", err)
	}
	if !strings.Contains(string(out), "hello-from-sandbox") {
		t.Errorf("output = %q, want to contain sentinel", string(out))
	}
}

func TestApplyPlatformSandbox_Darwin_GoTLSHandshakeUnderUsersDeny(t *testing.T) {
	// Contract (regression for the linear-pp-cli TLS failure):
	// a Go binary installed under /Users must be able to complete
	// the platform-side of a TLS handshake even when the profile
	// denies /Users wholesale. macOS Security framework reads the
	// caller binary's directory to scope code-signing identity
	// before issuing the SSL trust policy; the auto-allow in
	// buildSBPLProfile re-grants that read.
	//
	// The test compiles a tiny HTTPS client into a directory under
	// $HOME (so the deny rule would apply), points it at an
	// httptest.NewTLSServer, and asserts the failure mode is a
	// normal "unknown authority" cert error — NOT
	// "SecPolicyCreateSSL error: 0" (the symptom when the platform
	// verifier itself can't initialize). The self-signed test cert
	// is rejected either way; what we're proving is that the
	// platform verifier ran at all.
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	binDir := filepath.Join(home, "aileron-sandbox-tls-test")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bindir: %v", err)
	}
	defer os.RemoveAll(binDir)

	srcPath := filepath.Join(binDir, "main.go")
	const src = `package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	resp, err := http.Get(os.Args[1])
	if err != nil {
		fmt.Println("ERR:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	fmt.Println("OK")
}
`
	if err := os.WriteFile(srcPath, []byte(src), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	binPath := filepath.Join(binDir, "tls-probe")
	build := exec.CommandContext(context.Background(), "go", "build", "-o", binPath, srcPath)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// Spin up a TLS server with a self-signed cert. The probe will
	// fail TLS verification (cert isn't trusted), but the FAILURE
	// SHAPE is what we care about: a real x509 error, not a
	// SecPolicyCreateSSL initialization failure.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cmd := exec.CommandContext(context.Background(), binPath, srv.URL)
	limits := SpawnLimits{
		// No FSRead/FSWrite — exercise the auto-allow path in
		// isolation. The /Users deny applies; only the binary's
		// own directory should be re-granted by the auto-allow.
		// ProxyAddr is empty: SBPL will deny network*, but the
		// loopback test server lives in the same process and is
		// reached over loopback. To allow that, we set ProxyAddr
		// to the server's host:port so the network allow covers
		// the test path. This is a test-only convenience; in
		// production the proxy is the daemon's CONNECT proxy.
		ProxyAddr: strings.TrimPrefix(srv.URL, "https://"),
	}
	if _, err := applyPlatformSandbox(cmd, SpawnEnvelope{Program: binPath}, limits); err != nil {
		t.Fatalf("applyPlatformSandbox: %v", err)
	}
	out, _ := cmd.CombinedOutput()
	got := string(out)
	if strings.Contains(got, "SecPolicyCreateSSL") {
		t.Fatalf("TLS handshake never initialized — Security framework can't read the binary's directory:\n%s\nprofile state:\n%s",
			got, buildSBPLProfile(SpawnEnvelope{Program: binPath}, limits))
	}
	// Sanity: the probe should have reached the TLS layer. Either
	// it succeeded against the test server (unlikely without
	// custom RootCAs) or it failed with a cert-trust error.
	if !strings.Contains(got, "OK") &&
		!strings.Contains(got, "certificate") &&
		!strings.Contains(got, "x509") &&
		!strings.Contains(got, "tls:") {
		t.Fatalf("unexpected probe output (expected TLS-layer outcome):\n%s", got)
	}
}

func TestSBPLQuote_EscapesSpecialChars(t *testing.T) {
	// Contract: backslash and double-quote are escaped; everything
	// else passes through unchanged.
	cases := []struct {
		in, want string
	}{
		{`/usr/bin/git`, `"/usr/bin/git"`},
		{`/path with spaces`, `"/path with spaces"`},
		{`/has"quote`, `"/has\"quote"`},
		{`/has\backslash`, `"/has\\backslash"`},
	}
	for _, tc := range cases {
		if got := sbplQuote(tc.in); got != tc.want {
			t.Errorf("sbplQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- helpers ---

func mustContain(t *testing.T, haystack string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(haystack, n) {
			t.Errorf("expected substring %q in:\n%s", n, haystack)
		}
	}
}

func sliceEq(a, b []string) bool {
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
