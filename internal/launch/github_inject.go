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
	"github.com/ALRubinger/aileron/internal/sentinel"
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

// gitCredentialHelperConfig is the static, token-independent, and
// secret-free gitconfig mounted at /home/agent/.gitconfig. Under the
// ADR-0019 sealing model (#1195) the GitHub token never enters the
// container: it is resolved daemon-side and injected at the TLS
// forward-proxy boundary by the user/github host bindings. This file
// therefore carries no secret and no reference to `gh`.
//
// The helper is a no-op that returns empty credentials. Resetting the
// helper list (the bare `helper =`) clears any inherited helper, and
// the empty-credential shell helper makes git emit a fully-formed but
// *unauthenticated* HTTPS request instead of blocking on an interactive
// prompt or shelling out to `gh`. The daemon proxy then seals that
// request at egress. Because the content never changes and holds no
// secret, the mount is read-only and identical on every launch.
const gitCredentialHelperConfig = "[credential \"https://github.com\"]\n\thelper =\n\thelper = \"!f() { test \\\"$1\\\" = get && exit 0; }; f\"\n"

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
	// EnvAdditions merge into the agent's env. Under the ADR-0019
	// sealing model this never carries a GitHub secret. It does carry a
	// non-secret GH_TOKEN set to sentinel.GitHubTokenSentinel when a
	// usable user/github entry exists: `gh` short-circuits locally
	// without a token in its env, so emit-mechanism A (plant nothing,
	// seal the emitted request) cannot work for `gh`. The sentinel is a
	// format-mimicking placeholder that passes `gh`'s local validation so
	// `gh` issues its request; the daemon recognizes the sentinel at the
	// TLS boundary and swaps in the real credential (emit-mechanism B,
	// #1196). The sentinel is non-secret and safe to place in the env.
	EnvAdditions map[string]string

	// Mounts append to the sandbox volume list. Carries the individual
	// secret-free gitconfig file mount when a usable user/github entry
	// was found; empty otherwise. The mount holds no secret regardless.
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

// prepareGitHubInject probes the user-level GitHub entry in the vault
// and, when a usable token is present, mounts a read-only, secret-free
// no-op gitconfig at /home/agent/.gitconfig.
//
// Under the ADR-0019 sealing model (#1195) the token never enters the
// container: no GH_TOKEN env is set and the gitconfig carries no secret
// bytes. The mounted helper makes git emit a fully-formed but
// unauthenticated HTTPS request that the daemon proxy seals at egress
// via the user/github host bindings. The vault probe is retained only
// to drive the locked-vault warning UX and to gate the (secret-free)
// mount the same way the pre-sealing code did, keeping the prep shape
// stable; the daemon binding independently handles the locked/empty
// case fail-closed, so a missed mount never leaks or breaks auth.
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
	// GH_TOKEN carries the non-secret sentinel, never the real token.
	// `gh` short-circuits locally without a token, so we plant the
	// sentinel to make it issue its request; the daemon recognizes the
	// sentinel at the TLS boundary and swaps in the real user/github
	// credential (emit-mechanism B, #1196). The real secret never enters
	// the container in env, mount, or args. The mount below carries only
	// the secret-free no-op git credential helper (emit-mechanism A for
	// git-over-HTTPS, which the daemon seals from the unauthenticated
	// request git emits).
	prep.EnvAdditions["GH_TOKEN"] = sentinel.GitHubTokenSentinel
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
