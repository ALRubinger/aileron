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

	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/proxybinding"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
	"github.com/ALRubinger/aileron/internal/vault"
)

// githubUserService is the single hardcoded user-level service name the
// injector reads from the vault (`user/github`). The route shape lives
// in the daemon client; this constant names only the service slug.
const githubUserService = "github"

// githubCredentialRef is the vault path the user-level GitHub token lives
// at (`user/github`). The launcher resolves it to gate the no-op
// gitconfig warnings and to reuse the resolved token when planting the
// GitHub mechanism-B binding's sentinel, avoiding a second daemon call.
const githubCredentialRef = "user/github"

// userRefPrefix is the namespace prefix of a user-level credential ref
// (`user/<service>`). The launcher derives the service slug a
// mechanism-B binding's sentinel is gated on by trimming this prefix.
const userRefPrefix = "user/"

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
	// sealing model this never carries a secret. It does carry the
	// non-secret sentinel of each mechanism-B binding whose credential
	// resolves to a usable vault entry: a client that short-circuits
	// locally without a token (e.g. `gh`) issues its request only when a
	// format-mimicking placeholder passes its local validation. Each
	// planted pair is the binding's own SentinelEnv set to its
	// SentinelValue (#1247), so the GitHub binding still plants GH_TOKEN
	// with sentinel.GitHubTokenSentinel byte-for-byte. The daemon
	// recognizes the sentinel at the TLS boundary and swaps in the real
	// credential (emit-mechanism B, #1196). The sentinel is non-secret and
	// safe to place in the env.
	EnvAdditions map[string]string

	// Mounts append to the sandbox volume list. Always carries the
	// individual secret-free no-op gitconfig file mount, regardless of
	// vault state (entry present, missing, locked, or empty token). The
	// gitconfig content is invariant and holds no secret, so it is mounted
	// on every launch so git-over-HTTPS emits a completable unauthenticated
	// request instead of blocking on a prompt or shelling out to `gh`.
	Mounts []sandboxcontainer.Volume

	// Cleanup removes the transient host directory holding the rendered
	// gitconfig. Always non-nil and safe to call (a no-op when no
	// directory was created).
	Cleanup func()
}

// renderNoopGitconfigMount creates the transient host directory, writes
// the static secret-free no-op gitconfig, applies the chown hook, and
// returns a prep carrying the read-only file mount and a real Cleanup.
// EnvAdditions is initialized empty; the caller adds GH_TOKEN only on the
// token-present path. This is invoked on EVERY launch path (entry
// present, missing, locked, empty token) because the gitconfig content
// is invariant and secret-free regardless of vault state (Key Decision 4,
// #1195): always mounting it lets git-over-HTTPS emit a completable
// unauthenticated request the daemon seals at egress, instead of blocking
// on a prompt or shelling out to `gh`.
//
// chownHook, when non-nil, is applied to the transient directory before
// the mount is assembled. On rootful Docker Linux the host operator owns
// the 0700 transient dir, which the in-container agent UID cannot
// traverse; the hook chowns the tree to the agent UID so git-over-HTTPS
// can read the helper line. A hook failure is non-fatal: the injector
// warns and proceeds, since a same-UID environment needs no chown.
func renderNoopGitconfigMount(
	sessionLog *slog.Logger,
	stderr io.Writer,
	chownHook func(dir string) error,
) (githubInjectPrep, error) {
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

	return githubInjectPrep{
		EnvAdditions: map[string]string{},
		Mounts: []sandboxcontainer.Volume{{
			Source:   hostPath,
			Target:   githubConfigTarget,
			ReadOnly: true,
		}},
		Cleanup: cleanup,
	}, nil
}

// prepareGitHubInject always mounts a read-only, secret-free no-op
// gitconfig at /home/agent/.gitconfig — on every path, whether the GitHub
// entry is present, missing, the vault is locked, or the token is empty
// (Key Decision 4, #1195). The gitconfig content is invariant and carries
// no secret bytes, so mounting it unconditionally leaks nothing while
// ensuring git-over-HTTPS always emits a completable unauthenticated
// request the daemon proxy seals at egress via the user/github host
// bindings, instead of blocking on a prompt or shelling out to `gh`.
//
// It then plants the sentinel of each mechanism-B binding in mechBBindings
// whose credential resolves to a usable vault entry (#1247): the binding's
// non-secret SentinelValue is set into its SentinelEnv. The GitHub binding
// still plants GH_TOKEN with sentinel.GitHubTokenSentinel byte-for-byte;
// any additional B binding plants its own pair independently. The real
// credential never enters the container: it is resolved daemon-side and
// injected at the TLS boundary.
//
// Under the ADR-0019 sealing model (#1195) the token never enters the
// container in env, mount, or args. The GitHub vault probe drives the
// locked-vault warning UX and supplies the reused GitHub token; it no
// longer gates the mount, and each B binding's sentinel is gated on that
// binding's own credential resolving.
//
// Skip-clean semantics: a missing entry (ErrUserCredentialsNotFound) or a
// locked vault (vault.ErrCredentialUnavailable) still mounts the no-op
// gitconfig with no error and plants no sentinel for the unresolved
// binding — the launch proceeds with those operations unauthenticated. A
// token that is empty after trimming is treated the same way. Only an
// unexpected daemon error aborts. The locked-vault warning is still
// emitted.
//
// chownHook is threaded into renderNoopGitconfigMount; see its doc for
// the rootful-Docker-Linux rationale and the non-fatal-on-failure
// contract.
func prepareGitHubInject(
	ctx context.Context,
	daemon userCredsDaemon,
	mechBBindings []binding.HostBinding,
	sessionLog *slog.Logger,
	stderr io.Writer,
	chownHook func(dir string) error,
) (githubInjectPrep, error) {
	// Resolve the GitHub user credential first: it drives the locked-vault
	// warning UX and is reused (without a second daemon call) when planting
	// the GitHub mechanism-B binding's sentinel below. A missing entry or a
	// locked vault is a clean skip for GitHub (githubToken stays empty, so
	// the GitHub sentinel is gated out), never a launch abort; only an
	// unexpected daemon error aborts.
	var githubToken string
	secret, err := daemon.GetUserCredentials(ctx, githubUserService)
	switch {
	case err == nil:
		githubToken = strings.TrimSpace(string(secret.Value))
	case errors.Is(err, ErrUserCredentialsNotFound):
		// No user/github entry — proceed unauthenticated.
	case errors.Is(err, vault.ErrCredentialUnavailable):
		// Vault locked — no token available this launch. Warn so the user
		// knows GitHub ops will be unauthenticated, but never abort.
		githubInjectWarn(sessionLog, stderr,
			fmt.Errorf("vault locked; GitHub operations in the sandbox will be unauthenticated until you unlock the vault"))
	default:
		return githubInjectPrep{}, fmt.Errorf("read user github credentials: %w", err)
	}

	// Always mount the secret-free no-op gitconfig (mechanism A for
	// git-over-HTTPS) on every path, regardless of vault state.
	prep, err := renderNoopGitconfigMount(sessionLog, stderr, chownHook)
	if err != nil {
		return githubInjectPrep{}, err
	}

	// Plant each mechanism-B binding's sentinel into its env, gated on the
	// binding's credential resolving to a usable token. The GitHub token
	// resolved above is reused so the GitHub binding is not probed twice;
	// any other B binding's credential is resolved fresh and independently.
	plantSentinels(ctx, daemon, &prep, mechBBindings, githubToken)
	return prep, nil
}

// plantSentinels plants each mechanism-B binding's non-secret sentinel
// value into its env var, but only when the binding's credential
// resolves to a usable token (the swap has nothing to swap to otherwise).
// A binding's sentinel is non-secret, so the planted env never carries a
// secret. The real credential is resolved daemon-side and injected at the
// TLS boundary; it never enters the container.
//
// githubToken is the already-resolved user/github token (trimmed, may be
// empty): a binding whose credential ref is user/github reuses it rather
// than issuing a second daemon call. Any other ref is resolved here; an
// unavailable, missing, or empty credential plants nothing for that
// binding (a clean skip, no error).
func plantSentinels(
	ctx context.Context,
	daemon userCredsDaemon,
	prep *githubInjectPrep,
	mechBBindings []binding.HostBinding,
	githubToken string,
) {
	for _, hb := range mechBBindings {
		if hb.EmitMechanism != binding.EmitMechanismB {
			continue
		}
		// A B binding with no sentinel shape cannot be planted. The
		// constructor rejects this, so it is a defensive skip.
		if hb.SentinelValue == "" || hb.SentinelEnv == "" {
			continue
		}
		if hasUsableCredential(ctx, daemon, hb.CredentialRef, githubToken) {
			prep.EnvAdditions[hb.SentinelEnv] = hb.SentinelValue
		}
	}
}

// hasUsableCredential reports whether credentialRef resolves to a
// non-empty token. The user/github ref reuses the already-resolved
// githubToken to avoid a redundant daemon call. Any other user/<service>
// ref is resolved through the daemon; a missing entry, a locked vault, or
// an empty-after-trim token all report false (a clean skip). A ref
// outside the user/<service> namespace reports false: the launcher only
// resolves user-level credentials, so a connector-style ref plants no
// sentinel here.
func hasUsableCredential(ctx context.Context, daemon userCredsDaemon, credentialRef, githubToken string) bool {
	if credentialRef == githubCredentialRef {
		return githubToken != ""
	}
	service, ok := strings.CutPrefix(credentialRef, userRefPrefix)
	if !ok || service == "" {
		return false
	}
	secret, err := daemon.GetUserCredentials(ctx, service)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(secret.Value)) != ""
}

// mechanismBHostBindings assembles the canonical host-binding table the
// daemon recognizer reads (proxybinding.AllHostBindings: GitHub Go
// bindings plus descriptor bindings) and returns only the mechanism-B
// subset the planter plants sentinels for. Sharing the assembly keeps the
// launch-side plant and the proxy-side match reading one source of truth
// across the process boundary (#1247). A malformed descriptor surfaces an
// error rather than degrading to an empty plant set.
func mechanismBHostBindings() ([]binding.HostBinding, error) {
	all, err := proxybinding.AllHostBindings(proxybinding.DefaultLoadOptions())
	if err != nil {
		return nil, err
	}
	var out []binding.HostBinding
	for _, hb := range all {
		if hb.EmitMechanism == binding.EmitMechanismB {
			out = append(out, hb)
		}
	}
	return out, nil
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
