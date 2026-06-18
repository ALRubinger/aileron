package launch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
	"github.com/ALRubinger/aileron/internal/vault"
)

// githubUserService is the single hardcoded user-level service name the
// injector reads from the vault (`user/github`). The route shape lives
// in the daemon client; this constant names only the service slug.
const githubUserService = "github"

// githubConfigTarget is where the credential-helper gitconfig is mounted
// inside the container. It sits directly under /home/agent — the same
// parent as the workspace mount in v4 — so it must be delivered as an
// individual file mount, not a directory mount, mirroring Claude's
// .claude.json carve-out so the workspace mount is not masked.
const githubConfigTarget = "/home/agent/.gitconfig"

// gitCredentialHelperConfig is the static, token-independent gitconfig
// that wires git's HTTPS credential helper to `gh`. The token never
// appears in this file; it flows only via the GH_TOKEN env var, which
// `gh auth git-credential` reads at request time. Because the content
// never changes, the mount is read-only.
const gitCredentialHelperConfig = "[credential \"https://github.com\"]\n\thelper = !gh auth git-credential\n"

// userCredsDaemon is the one-method subset of *daemonClient the GitHub
// injector depends on. Defined as an interface so tests substitute a
// fake without spinning up an httptest server.
type userCredsDaemon interface {
	GetUserCredentials(ctx context.Context, service string) (vault.Secret, error)
}

// githubInjectPrep is the output of prepareGitHubInject — the env and
// mount additions the launcher merges into the sandbox launch, plus a
// cleanup for the transient host directory. Its shape mirrors
// authSpecPrep but is deliberately separate: this injector is
// user-level and runs on every sandbox launch independent of the
// per-agent AuthSpec.
type githubInjectPrep struct {
	// EnvAdditions merge into the agent's env. Carries GH_TOKEN when a
	// usable token was found; empty otherwise.
	EnvAdditions map[string]string

	// Mounts append to the sandbox volume list. Carries the individual
	// gitconfig file mount when a token was found; empty otherwise.
	Mounts []sandboxcontainer.Volume

	// Cleanup removes the transient host directory holding the rendered
	// gitconfig. Always non-nil and safe to call (a no-op when no
	// directory was created).
	Cleanup func()
}

// emptyGitHubInjectPrep returns a clean, no-op prep: no env, no mount,
// and a nil-safe Cleanup. Used on every skip path (no entry, locked
// vault, empty token) so the caller can unconditionally defer Cleanup
// and range over EnvAdditions/Mounts.
func emptyGitHubInjectPrep() githubInjectPrep {
	return githubInjectPrep{
		EnvAdditions: map[string]string{},
		Cleanup:      func() {},
	}
}

// prepareGitHubInject reads the user-level GitHub token from the vault
// and, when present, produces a GH_TOKEN env addition plus a read-only
// bind mount of a static git credential-helper gitconfig at
// /home/agent/.gitconfig.
//
// Skip-clean semantics: a missing entry (ErrUserCredentialsNotFound) or
// a locked vault (vault.ErrCredentialUnavailable) returns an empty prep
// with no error — the launch proceeds with GitHub operations simply
// unauthenticated. A token that is empty after trimming is treated the
// same way. Only an unexpected daemon error aborts.
//
// chownHook, when non-nil, is applied to the transient directory before
// the mount is added. On rootful Docker Linux the host operator owns
// the 0700 transient dir, which the in-container agent UID cannot
// traverse; the hook chowns the tree to the agent UID so git-over-HTTPS
// can read the helper line. A hook failure is non-fatal: the injector
// warns and proceeds, since a same-UID environment needs no chown.
func prepareGitHubInject(
	ctx context.Context,
	daemon userCredsDaemon,
	sessionLog *slog.Logger,
	stderr io.Writer,
	chownHook func(dir string) error,
) (githubInjectPrep, error) {
	secret, err := daemon.GetUserCredentials(ctx, githubUserService)
	if err != nil {
		switch {
		case errors.Is(err, ErrUserCredentialsNotFound):
			// No user/github entry — proceed unauthenticated.
			return emptyGitHubInjectPrep(), nil
		case errors.Is(err, vault.ErrCredentialUnavailable):
			// Vault locked — no token available this launch. Warn so the
			// user knows GitHub ops will be unauthenticated, but never
			// abort the launch.
			githubInjectWarn(sessionLog, stderr,
				fmt.Errorf("vault locked; GitHub operations in the sandbox will be unauthenticated until you unlock the vault"))
			return emptyGitHubInjectPrep(), nil
		default:
			return githubInjectPrep{}, fmt.Errorf("read user github credentials: %w", err)
		}
	}

	token := strings.TrimSpace(string(secret.Value))
	if token == "" {
		// An entry with no usable token is equivalent to no entry.
		return emptyGitHubInjectPrep(), nil
	}

	dir, err := os.MkdirTemp("", "aileron-github-inject-")
	if err != nil {
		return githubInjectPrep{}, fmt.Errorf("create github inject dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	// 0700 (not 0600): the dir needs its traverse bit so the mounted
	// file beneath it is reachable. The in-container agent reaches the
	// file via the chown hook below on rootful Docker Linux.
	if err := os.Chmod(dir, 0o700); err != nil {
		cleanup()
		return githubInjectPrep{}, fmt.Errorf("chmod github inject dir: %w", err)
	}

	hostPath := filepath.Join(dir, ".gitconfig")
	if err := os.WriteFile(hostPath, []byte(gitCredentialHelperConfig), 0o644); err != nil {
		cleanup()
		return githubInjectPrep{}, fmt.Errorf("write github inject gitconfig: %w", err)
	}

	// Chown the transient tree to the in-container agent UID before the
	// mount is added. Non-fatal on failure: warn and proceed, since a
	// same-UID environment (macOS/Windows shim, or a root-owned image)
	// needs no chown and the hook is nil there anyway.
	if chownHook != nil {
		if err := chownHook(dir); err != nil {
			githubInjectWarn(sessionLog, stderr,
				fmt.Errorf("chown github inject dir to image agent UID: %w; the agent runs as a non-root system user while the host operator owns this bind-mounted dir, so git-over-HTTPS may be unable to read the credential helper line (EPERM) — the remedy is the chown-to-agent-UID fix on the gitconfig bind mount", err))
		}
	}

	prep := emptyGitHubInjectPrep()
	prep.EnvAdditions["GH_TOKEN"] = token
	prep.Mounts = []sandboxcontainer.Volume{{
		Source:   hostPath,
		Target:   githubConfigTarget,
		ReadOnly: true,
	}}
	prep.Cleanup = cleanup
	return prep, nil
}

// githubInjectWarn emits a non-fatal diagnostic to the session log and
// the user's stderr, matching the captureWarn one-liner shape used by
// the auth-spec runtime.
func githubInjectWarn(log *slog.Logger, stderr io.Writer, err error) {
	if log != nil {
		log.Warn("github inject", "error", err)
	}
	if stderr != nil {
		fmt.Fprintf(stderr, "[launcher] github inject: %v\n", err)
	}
}
