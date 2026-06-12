//go:build integration_sandbox

// Proxy-CA wrapper argument-passthrough integration test.
//
// When sandbox proxy bootstrap is active the launcher prefixes the
// in-container command with `aileron-run-with-proxy-ca` and runs the
// container as root; the wrapper installs the session CA and then drops
// to the unprivileged `agent` user to exec the real agent command. That
// command routinely carries `--`-prefixed flags (Claude:
// `--allowedTools ...`, `--dangerously-skip-permissions`).
//
// The wrapper must pass those flags through to the agent untouched. An
// earlier implementation dropped privileges with BusyBox `su -c`, whose
// getopt scans the whole argv and rejected the inner command's long
// flags ("su: unrecognized option: allowedTools"), aborting the launch
// before the agent ever started. This test reproduces that failure mode
// on the real Alpine base image: it builds sandbox-base, runs the
// wrapper with a flagged command, and asserts the flags reach the inner
// command while it runs as the unprivileged agent user.
//
// Unlike the AuthSpec bind-mount test this is not Linux-only — the bug
// lives in the container's BusyBox userland, which is identical across
// hosts — so it runs anywhere a Docker runtime is available.
//
// Run with:
//
//	go test -tags=integration_sandbox -run TestRunWithProxyCAPassesFlaggedCommand ./internal/launch/...
package launch

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sandboxcomposition "github.com/ALRubinger/aileron/internal/sandbox/composition"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

const runWithProxyCATestImage = "aileron/sandbox-base:run-with-proxy-ca-test"

func TestRunWithProxyCAPassesFlaggedCommand(t *testing.T) {
	rt, err := sandboxcontainer.ResolveRuntime("docker")
	if err != nil {
		t.Skipf("no docker runtime on PATH: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	buildRunWithProxyCABaseImage(ctx, t, rt)

	// The wrapper aborts unless the CA install succeeds, so feed it a real
	// single-certificate PEM bind-mounted into the container.
	caDir := t.TempDir()
	caPath := filepath.Join(caDir, "ca.pem")
	writeSelfSignedCAPEM(t, caPath)
	const containerCAPath = "/tmp/aileron-test-ca.pem"

	// Echo the dropped-to user and the positional args the inner shell
	// received. `$*` joins $1.. — i.e. everything after the `_` argv0 —
	// which is exactly the flag list the wrapper must forward verbatim.
	const innerScript = `printf 'user=%s\nargs=%s\n' "$(id -un)" "$*"`

	args := []string{
		"run", "--rm",
		"--user", "root",
		"--volume", caPath + ":" + containerCAPath + ":ro",
		"--env", "AILERON_SANDBOX_PROXY_CA_FILE=" + containerCAPath,
		"--entrypoint", "/usr/local/bin/aileron-run-with-proxy-ca",
		runWithProxyCATestImage,
		"/bin/sh", "-c", innerScript, "_",
		"--allowedTools", "Bash(*) mcp__aileron",
		"--dangerously-skip-permissions",
	}
	var stdout, stderr bytes.Buffer
	c := exec.CommandContext(ctx, rt, args...)
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		t.Fatalf("aileron-run-with-proxy-ca failed to launch a flagged command: %v\nstdout=%q\nstderr=%q\n\nThis is the regression: the wrapper must drop privileges with a tool that execs the command directly (su-exec). BusyBox `su -c` parses the inner command's `--`-flags as its own options and aborts the launch.", err, stdout.String(), stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "user=agent") {
		t.Fatalf("command did not run as the unprivileged `agent` user\nstdout=%q\nstderr=%q", out, stderr.String())
	}
	// Load-bearing assertion: every `--`-prefixed flag reached the inner
	// command rather than being swallowed by the privilege-drop tool.
	for _, want := range []string{"--allowedTools", "Bash(*) mcp__aileron", "--dangerously-skip-permissions"} {
		if !strings.Contains(out, want) {
			t.Fatalf("flag %q did not reach the inner command — the privilege-drop tool consumed it\nstdout=%q\nstderr=%q", want, out, stderr.String())
		}
	}
}

// buildRunWithProxyCABaseImage builds the sandbox-base image under a
// dedicated test tag. AILERON_SANDBOX_BASE_CONTEXT may point at the
// images/sandbox-base context; otherwise the Builder walks up from cwd.
func buildRunWithProxyCABaseImage(ctx context.Context, t *testing.T, rt string) {
	t.Helper()
	builder := sandboxcontainer.Builder{
		Runtime: rt,
		Stdout:  testLogWriter{t},
		Stderr:  testLogWriter{t},
	}
	plan := sandboxcomposition.Plan{
		Tier:  sandboxcomposition.TierBase,
		Image: runWithProxyCATestImage,
	}
	if _, err := builder.Build(ctx, sandboxcontainer.BuildOptions{
		Plan:   plan,
		Tag:    runWithProxyCATestImage,
		Policy: sandboxcontainer.BuildPolicyAlways,
	}); err != nil {
		t.Fatalf("build sandbox-base test image: %v (set AILERON_SANDBOX_BASE_CONTEXT to images/sandbox-base if the context was not found)", err)
	}
}

// writeSelfSignedCAPEM writes a single self-signed CA certificate to
// path so aileron-install-proxy-ca has a valid, non-empty PEM to install.
func writeSelfSignedCAPEM(t *testing.T, path string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "aileron-test-proxy-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create self-signed CA: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o644); err != nil {
		t.Fatalf("write CA pem: %v", err)
	}
}
