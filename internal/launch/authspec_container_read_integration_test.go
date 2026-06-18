//go:build integration_sandbox

// In-container credential read + capture round-trip integration test (#1025).
//
// This closes the verification gap that sandbox-agent-auth.md names and
// issue #1025 tracks: the host side of vault-backed agent auth (ADR-0025)
// is covered by unit tests — each agent's *_auth_test.go asserts the
// ContainerPath shape and the Render/Capture byte round-trip, and the
// prepareAuthSpec tests cover the host-side reclaim → read → freshness-gated
// PUT — but no automated test had ever started a REAL container, had the
// in-container process read the rendered credential at its documented
// ContainerPath, and asserted that an in-container rotation round-trips
// back to the vault. Step 3 ("Run") of the launch lifecycle was unverified
// by CI; the only coverage was a manual smoke-check in the docs.
//
// What this test exercises, end to end against a live Docker container:
//
//	prepareAuthSpec (Render → transient dir → writable parent-dir mount)
//	  → container starts as the image's non-root `agent` user
//	  → the agent READS the credential at ContainerPath and asserts it
//	    matches the seeded envelope (this is the previously-unverified
//	    in-container read path)
//	  → the agent ROTATES it the way Claude does (tmpfile + rename)
//	  → clean exit → prep.CaptureFn reclaims ownership, reads the rotated
//	    bytes, runs Capture, gates on Fresher, and PUTs to the vault.
//
// The AuthSpec here is a Claude-shaped FIXTURE, not agents.Claude{}.AuthSpec():
// the `agents` package imports `launch`, so `launch` cannot import `agents`,
// and prepareAuthSpec is unexported. The fixture reuses Claude's real
// ContainerPath, vault path, byte-identity Render, and an expiresAt-based
// Fresher so the lifecycle contract is exercised faithfully. Per-agent
// envelope correctness stays covered by agents/claude_auth_test.go; what
// only a container can verify — the read at ContainerPath and the
// rotation-capture round-trip — lives here.
//
// FAIL-FAST: the dedicated integration_sandbox CI lane provisions Docker, so
// a missing runtime is a job-configuration failure, not a t.Skip (see #1082,
// #1086). The test runs on any Docker host: on Linux it supplies the
// chown-to-agent-UID hook the launcher uses on rootful Docker; on
// macOS/Windows the hook is nil exactly as the launcher passes nil, since
// Docker Desktop's file-sharing shim translates UIDs.
//
// Run with:
//
//	go test -tags=integration_sandbox -run TestAuthSpecInContainerCredentialReadCapture ./internal/launch/...
package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
	"github.com/ALRubinger/aileron/internal/vault"
)

// claudeCredsContainerPath mirrors agents.claudeCredentialsContainerPath.
// The mount the launcher builds for a Claude-shaped FileBinding (no
// MountAsFile) is the writable parent dir /home/agent/.claude, so the agent
// can rotate .credentials.json via tmpfile + rename inside the container.
const claudeCredsContainerPath = "/home/agent/.claude/.credentials.json"

// claudeVaultName is the short agent name prepareAuthSpec derives from the
// "agents/claude/oauth" VaultPath and uses for daemon GET/PUT.
const claudeVaultName = "claude"

func TestAuthSpecInContainerCredentialReadCapture(t *testing.T) {
	rt, err := sandboxcontainer.ResolveRuntime("docker")
	if err != nil {
		// FAIL-FAST: the integration_sandbox lane provisions Docker; its
		// absence is a job-configuration failure, not a skip (#1082/#1086).
		t.Fatalf("no docker runtime on PATH (required for the integration_sandbox lane, not skipped): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAuthSpecBaseImage(ctx, t, rt)

	runner := sandboxcontainer.DefaultRunner()
	agentUID, err := resolveAgentUID(ctx, runner, rt, authSpecBindMountTestImage)
	if err != nil {
		t.Fatalf("resolve agent UID for %s: %v", authSpecBindMountTestImage, err)
	}
	if agentUID == 0 {
		t.Fatalf("sandbox-base image resolved agent UID 0 (root); the image's USER directive should select the non-root `agent` user — the in-container read contract is meaningless as root")
	}
	hostUID := os.Getuid()

	// Mirror the launcher's platform split: Linux supplies a
	// chown-to-agent-UID hook (rootful Docker preserves host ownership
	// through a writable bind mount, so the non-root agent cannot even READ
	// a host-owned 0600 credential without it, let alone rotate it);
	// macOS/Windows pass nil because Docker Desktop's file-sharing shim
	// translates UIDs.
	var chownFn, reclaimFn func(string) error
	if runtime.GOOS == "linux" {
		chownFn = func(dir string) error {
			return chownTreeViaRuntime(ctx, runner, rt, authSpecBindMountTestImage, dir, agentUID)
		}
		reclaimFn = func(dir string) error {
			return chownTreeViaRuntime(ctx, runner, rt, authSpecBindMountTestImage, dir, hostUID)
		}
	}

	// rotation_is_captured_to_vault is the load-bearing case: the agent
	// reads the seeded credential, rotates it to a newer envelope, and the
	// rotation must round-trip to the vault on clean exit.
	t.Run("rotation_is_captured_to_vault", func(t *testing.T) {
		seeded := claudeEnvelope("access-v1", 1000)
		rotated := claudeEnvelope("access-v2", 2000)

		daemon := newFakeDaemon()
		daemon.seed(claudeVaultName, seeded)

		prep, err := prepareAuthSpec(ctx, claudeVaultName, claudeFixtureAuthSpec(), daemon, newTestLogger(), io.Discard, chownFn, reclaimFn)
		if err != nil {
			t.Fatalf("prepareAuthSpec: %v", err)
		}
		defer prep.Cleanup()

		// The container asserts the read matches the seeded credential
		// (proving the rendered file is readable at ContainerPath inside the
		// sandbox), then rotates it via tmpfile + rename so capture has a
		// strictly-newer envelope to PUT.
		script := containerReadAssertThenRotate(claudeCredsContainerPath)
		env := map[string]string{"EXPECTED": string(seeded), "ROTATED": string(rotated)}
		stdout, stderr, runErr := runContainerWithMounts(ctx, rt, authSpecBindMountTestImage, prep.Mounts, env, script)
		if runErr != nil {
			t.Fatalf("in-container credential read/rotate FAILED: %v\nThis is the gap #1025 guards: the rendered credential must be readable at %s inside the sandbox and rotatable in place.\nstdout: %s\nstderr: %s", runErr, claudeCredsContainerPath, stdout, stderr)
		}

		// Capture-on-clean-exit: reclaim ownership, read the rotated file,
		// Capture, freshness-gate, PUT.
		prep.CaptureFn(ctx)

		if len(daemon.puts) != 1 {
			t.Fatalf("capture PUT count = %d, want 1 (the in-container rotation must round-trip to the vault); GETs=%v", len(daemon.puts), daemon.gets)
		}
		if got, want := string(daemon.puts[0].Secret.Value), string(rotated); got != want {
			t.Fatalf("captured vault value = %q, want the rotated envelope %q", got, want)
		}
		if got := daemon.puts[0].Name; got != claudeVaultName {
			t.Fatalf("captured under vault name %q, want %q", got, claudeVaultName)
		}
	})

	// clean_exit_without_rotation_does_not_clobber_vault proves the
	// freshness gate holds end-to-end in a container: an agent that reads
	// the credential and exits WITHOUT rotating must not trigger a PUT, so a
	// no-op session never clobbers a newer vault entry (the common
	// steady-state case after first login).
	t.Run("clean_exit_without_rotation_does_not_clobber_vault", func(t *testing.T) {
		seeded := claudeEnvelope("access-v1", 1000)

		daemon := newFakeDaemon()
		daemon.seed(claudeVaultName, seeded)

		prep, err := prepareAuthSpec(ctx, claudeVaultName, claudeFixtureAuthSpec(), daemon, newTestLogger(), io.Discard, chownFn, reclaimFn)
		if err != nil {
			t.Fatalf("prepareAuthSpec: %v", err)
		}
		defer prep.Cleanup()

		script := containerReadAssertOnly(claudeCredsContainerPath)
		env := map[string]string{"EXPECTED": string(seeded)}
		stdout, stderr, runErr := runContainerWithMounts(ctx, rt, authSpecBindMountTestImage, prep.Mounts, env, script)
		if runErr != nil {
			t.Fatalf("in-container credential read FAILED: %v\nstdout: %s\nstderr: %s", runErr, stdout, stderr)
		}

		prep.CaptureFn(ctx)

		if len(daemon.puts) != 0 {
			t.Fatalf("capture PUT count = %d, want 0 (a clean exit without rotation must not clobber the vault; the freshness gate should hold)", len(daemon.puts))
		}
	})
}

// claudeFixtureAuthSpec is a Claude-shaped AuthSpec built from the exported
// launch types. It reuses Claude's real ContainerPath, vault path, and
// byte-identity Render, with an expiresAt-based Fresher matching
// claudeFresher's newer-wins semantics. It is deliberately NOT
// agents.Claude{}.AuthSpec() — see the file header for the import-cycle
// reason — and carries only the FileBinding the lifecycle test needs.
func claudeFixtureAuthSpec() AuthSpec {
	return AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: claudeCredsContainerPath,
			Mode:          0o600,
			Required:      false,
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture: func(b []byte) (vault.Secret, error) {
				if _, err := envelopeExpiresAt(b); err != nil {
					return vault.Secret{}, fmt.Errorf("captured bytes are not a claude oauth envelope: %w", err)
				}
				return vault.Secret{Value: b, Metadata: vault.Metadata{Type: "oauth_refresh_token"}}, nil
			},
			Fresher: func(captured, current vault.Secret) (bool, error) {
				capturedExp, err := envelopeExpiresAt(captured.Value)
				if err != nil {
					return false, fmt.Errorf("parse captured envelope: %w", err)
				}
				currentExp, err := envelopeExpiresAt(current.Value)
				if err != nil {
					// Mirror claudeFresher: a malformed current entry is
					// unusable, so let the capture overwrite it.
					return false, ErrCurrentEnvelopeMalformed
				}
				return capturedExp > currentExp, nil
			},
		}},
	}
}

// claudeEnvelopeFixture mirrors the claudeAiOauth envelope shape that
// agents.claudeCredentialEnvelope defines, scoped to the fields this test
// exercises.
type claudeEnvelopeFixture struct {
	ClaudeAiOauth struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken,omitempty"`
		ExpiresAt    int64  `json:"expiresAt"`
	} `json:"claudeAiOauth"`
}

// claudeEnvelope marshals a deterministic claude oauth envelope. The bytes
// are byte-identical between the seed, the rendered file, and the EXPECTED
// env var the container compares against, because Render is byte-identity
// and json.Marshal is deterministic for a fixed struct.
func claudeEnvelope(accessToken string, expiresAt int64) []byte {
	var e claudeEnvelopeFixture
	e.ClaudeAiOauth.AccessToken = accessToken
	e.ClaudeAiOauth.RefreshToken = "rt-" + accessToken
	e.ClaudeAiOauth.ExpiresAt = expiresAt
	b, err := json.Marshal(e)
	if err != nil {
		panic(fmt.Sprintf("marshal claude envelope fixture: %v", err))
	}
	return b
}

func envelopeExpiresAt(b []byte) (int64, error) {
	var e claudeEnvelopeFixture
	if err := json.Unmarshal(b, &e); err != nil {
		return 0, err
	}
	if e.ClaudeAiOauth.AccessToken == "" {
		return 0, fmt.Errorf("envelope missing claudeAiOauth.accessToken")
	}
	return e.ClaudeAiOauth.ExpiresAt, nil
}

// containerReadAssertThenRotate returns a /bin/sh script that asserts the
// credential at path equals $EXPECTED, then rotates it to $ROTATED via
// tmpfile + rename (Claude's mid-session rotation pattern).
func containerReadAssertThenRotate(path string) string {
	return "set -e; " +
		`got=$(cat ` + path + `); ` +
		`if [ "$got" != "$EXPECTED" ]; then echo "in-container read mismatch at ` + path + `: got [$got] want [$EXPECTED]" >&2; exit 1; fi; ` +
		`printf '%s' "$ROTATED" > ` + path + `.tmp && mv ` + path + `.tmp ` + path
}

// containerReadAssertOnly returns a /bin/sh script that asserts the
// credential at path equals $EXPECTED and exits without modifying it.
func containerReadAssertOnly(path string) string {
	return "set -e; " +
		`got=$(cat ` + path + `); ` +
		`if [ "$got" != "$EXPECTED" ]; then echo "in-container read mismatch at ` + path + `: got [$got] want [$EXPECTED]" >&2; exit 1; fi`
}

// runContainerWithMounts runs a one-shot container as the image's default
// (agent) user with the prepared AuthSpec mounts and env, executing cmd via
// /bin/sh -c. It returns stdout, stderr, and the run error. Args are passed
// to the runtime via exec (no intermediate shell), so credential JSON in env
// values needs no escaping.
func runContainerWithMounts(ctx context.Context, rt, image string, mounts []sandboxcontainer.Volume, env map[string]string, cmd string) (string, string, error) {
	args := []string{"run", "--rm"}
	for _, m := range mounts {
		spec := m.Source + ":" + m.Target
		if m.ReadOnly {
			spec += ":ro"
		}
		args = append(args, "--volume", spec)
	}
	for k, v := range env {
		args = append(args, "--env", k+"="+v)
	}
	args = append(args, image, "/bin/sh", "-c", cmd)

	var stdout, stderr bytes.Buffer
	c := exec.CommandContext(ctx, rt, args...)
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	return stdout.String(), stderr.String(), err
}
