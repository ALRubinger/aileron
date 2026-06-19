package launch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"

	"github.com/ALRubinger/aileron/internal/oauth"
	"github.com/ALRubinger/aileron/internal/vault"
)

// ErrCurrentEnvelopeMalformed is wrapped by a Fresher when the freshly
// captured credential parses but the current vault entry does not. The
// capture path treats it as "captured wins": a corrupt current entry is
// already unusable (Render rejects it on the next launch), so a valid
// fresh capture overwrites it rather than being skipped. See ADR-0025's
// freshness-comparison section.
var ErrCurrentEnvelopeMalformed = errors.New("launch: current vault credential envelope is malformed")

// AuthSpec is an agent's declarative description of its credential
// bindings. The launcher consumes the spec around the agent's
// container lifecycle:
//
//   - On launch start, Render reads each binding's vault entry
//     (via the daemon) and materializes either env vars (EnvBindings)
//     or files in a writable bind-mount (FileBindings + StaticFiles).
//   - On clean container exit, Capture reads each FileBinding's
//     host-side file and snapshots the bytes back to the vault.
//     This persists in-container rotations (Claude Code rotates its
//     OAuth access token mid-session) and seeds the vault on first
//     launch (the agent's own interactive login writes the file the
//     first time; Capture stores it).
//
// AuthSpec is independent of seeding: Render and Capture never know
// whether the bytes arrived via in-container login + Capture, via
// `aileron vault put`, or via any future seeding path.
//
// See ADR-0025 for the full lifecycle, including the SIGINT/SIGTERM
// graceful-shutdown salvage path and the freshness-comparison rule
// that prevents stale captures from clobbering newer vault entries.
type AuthSpec struct {
	EnvBindings  []EnvBinding
	FileBindings []FileBinding
	StaticFiles  []StaticFile
}

// EnvBinding declares one env-var-shaped binding rendered from a
// single vault entry into a map of env vars that merge into the
// agent process's environment.
type EnvBinding struct {
	// VaultPath is the namespaced path the daemon looks up under
	// `agents/<name>/<purpose>`. The launcher resolves this through
	// the daemon HTTP API per ADR-0011/0012.
	VaultPath string

	// Required gates the launch on a vault miss. When true and the
	// vault returns vault_not_found, the launcher fails the launch
	// with an error naming the missing path. When false, Render is
	// skipped and the agent starts with no env additions from this
	// binding.
	Required bool

	// Render maps a resolved vault secret to one or more env vars.
	// Returning an error aborts the launch.
	Render func(vault.Secret) (map[string]string, error)
}

// FileBinding declares one file-shaped binding: a host-side file
// written under a writable bind-mount whose contents come from the
// vault and (optionally) flow back via Capture on clean exit.
//
// The mount target is derived from the binding's ContainerPath: the
// launcher bind-mounts the parent directory of ContainerPath (e.g.
// /home/agent/.claude/) at the host-side transient dir, so the file
// at ContainerPath plus any sibling files the agent writes share the
// same mount. This is the canonical layout Claude Code and Codex
// both expect — Claude rotates .credentials.json via a tmpfile +
// rename dance within .claude/, and Codex emits auth.json next to
// config.toml.
type FileBinding struct {
	// VaultPath is the namespaced vault entry the launcher reads/writes
	// (e.g. agents/claude/oauth).
	VaultPath string

	// ContainerPath is the absolute path the file appears at inside
	// the sandbox container (e.g. /home/agent/.claude/.credentials.json).
	ContainerPath string

	// Mode is the file mode the launcher writes the rendered bytes
	// with. Typically 0600 for credential files.
	Mode fs.FileMode

	// Required gates the launch on a vault miss. When true and the
	// vault returns vault_not_found, the launcher fails the launch.
	// When false (the typical case for "seeding on first launch"),
	// the bind-mount source is empty and the in-container agent
	// performs its normal interactive login.
	Required bool

	// Render maps the vault secret to the file bytes. The launcher
	// writes whatever bytes Render returns into the host-side file
	// at the binding's ContainerPath inside the bind-mount.
	Render func(vault.Secret) ([]byte, error)

	// Capture maps the post-run file bytes back to a vault secret.
	// Fired on clean container exit (Builder.Run returns nil) and
	// on SIGINT/SIGTERM after best-effort graceful container stop.
	// Returning an error skips the vault write for this binding;
	// the launcher logs a stderr warning so the user sees the
	// failure mode and recovery path.
	Capture func([]byte) (vault.Secret, error)

	// Fresher, when non-nil, gates the capture-time vault write on a
	// freshness comparison. CaptureFn calls it with the secret the
	// agent just rotated (captured) and the entry currently in the
	// vault (current); it must return true iff captured is strictly
	// newer than current. Only a true result triggers the PUT.
	//
	// A nil Fresher preserves last-writer-wins: every clean capture
	// PUTs unconditionally. This is the behavior all agents had before
	// the hook existed, so leaving Fresher nil is the safe default for
	// agents that never rotate their credential in-container.
	//
	// Returning a non-nil error skips the PUT and retains the prior
	// vault entry — a comparison the launcher cannot make safely must
	// never clobber the stored credential. See ADR-0025's
	// freshness-comparison section.
	//
	// The one carve-out: if the freshly captured envelope parses but the
	// *current* vault entry does not, the Fresher must wrap
	// [ErrCurrentEnvelopeMalformed]. A corrupt current entry is unusable
	// (Render rejects it on the next launch), so CaptureFn treats that
	// signal as "captured wins" and overwrites it rather than retaining
	// the corruption. A parse failure of the *captured* envelope still
	// returns a plain error so garbage is never written.
	Fresher func(captured, current vault.Secret) (bool, error)

	// PreLaunchRefresh, when non-nil, runs before Render. It
	// receives the resolved vault secret plus the daemon-backed
	// dependencies needed to refresh and persist a rotated token,
	// and returns the (possibly rotated) secret Render then uses.
	//
	// The hook must persist a rotated secret to the vault *before*
	// returning successfully — the AE6 invariant requires that the
	// rotated bundle is in vault before the container starts. A
	// failed persist aborts the launch.
	PreLaunchRefresh func(vault.Secret, RefreshDeps) (vault.Secret, error)

	// HostAcquire, when non-nil, is a host-side credential acquirer the
	// launcher invokes when the vault GET for this binding misses and
	// the binding is not Required. It runs on the host (browser +
	// loopback callback + token exchange, all pure Go) and returns the
	// vault.Secret the launcher then PUTs through the daemon and renders
	// to the bind-mounted file — exactly as a vault-resident credential
	// would be. No token is ever placed in the agent's environment; the
	// acquired Secret follows the same vault-seeded path as every other
	// credential (ADR-0025).
	//
	// Contract:
	//
	//   - The returned Secret must satisfy this same binding's Render.
	//     The launcher renders the acquired Secret immediately, so a
	//     malformed envelope surfaces as a launch-aborting Render error
	//     rather than seeding garbage into the vault.
	//
	//   - The acquirer must NOT assume the agent CLI is installed on the
	//     host. Acquisition is pure Go (HTTP + browser open); the
	//     launcher never shells out to the agent binary.
	//
	//   - A returned Secret with an empty Value, or a cancelled/failed
	//     acquire (non-nil error), is non-fatal: the launcher logs a
	//     warning and falls back to the legacy in-container login path
	//     (no file written; capture-on-exit seeds the vault). Only a
	//     daemon PUT failure or a Render failure of a successfully
	//     acquired Secret aborts the launch.
	//
	// A nil HostAcquire preserves the legacy in-container path. The
	// per-launch `--host-login` flag (and AILERON_LAUNCH_HOST_LOGIN env)
	// can force the legacy path even for a binding that declares an
	// acquirer.
	HostAcquire func(context.Context, HostAcquireDeps) (vault.Secret, error)

	// MountAsFile selects file-mount vs parent-dir mount strategy.
	//
	// When false (default), the launcher bind-mounts the binding's
	// container parent directory writable so the in-container agent
	// can rotate the credential via a tmpfile-and-rename dance
	// inside that directory (Claude Code's pattern for
	// .credentials.json).
	//
	// When true, the launcher bind-mounts only the binding's
	// individual file at ContainerPath. Use this when the agent
	// does not rotate the file in-container and the binding's
	// parent directory is also the target of another mount that
	// must coexist (Codex's ModeSandbox keeps config.toml mounted
	// via ConfigureMCP; we mount auth.json beside it as a file).
	MountAsFile bool
}

// StaticFile declares in-container state that is constant per agent
// and not vault-backed. Currently used for Claude's .claude.json
// onboarding stub: a fixed JSON blob telling Claude to skip its
// first-launch wizard regardless of vault state.
type StaticFile struct {
	// ContainerPath is the absolute path inside the sandbox container.
	ContainerPath string

	// Mode is the file mode the launcher writes with. Typically 0644
	// for agent state files that the in-container agent reads.
	Mode fs.FileMode

	// Content is the file bytes written unconditionally on every
	// launch.
	Content []byte
}

// RefreshDeps is the bag of helpers a FileBinding's PreLaunchRefresh
// hook needs to talk to the vendor's token endpoint and persist a
// rotated secret back through the daemon. The launcher fills it in
// before invoking the hook.
type RefreshDeps struct {
	// Ctx is the launch context, propagated so HTTP refresh
	// requests honor cancellation when the user kills the launch.
	Ctx context.Context

	// HTTPClient drives the refresh POST against the vendor's token
	// URL. Tests inject httptest-backed clients; production uses a
	// shared client with a sane timeout.
	HTTPClient *http.Client

	// PutAgentCredentials persists a rotated secret back to the vault
	// at the binding's VaultPath. The launcher binds this to the
	// daemon client so the hook never opens the vault file itself —
	// the daemon is the single trust boundary per ADR-0011.
	PutAgentCredentials func(secret vault.Secret) error
}

// HostAcquireDeps is the bag of helpers a FileBinding's HostAcquire
// hook needs to run a host-side OAuth flow: an HTTP client for the
// token exchange and a browser opener for the consent URL. The
// launcher fills it in before invoking the hook.
//
// Note the absence of a PutAgentCredentials helper: unlike
// RefreshDeps, the acquirer does not persist the credential itself.
// The launcher owns the PUT (and the immediate Render that validates
// the acquired Secret), so the acquirer cannot skip seeding or seed an
// envelope its own binding's Render would reject. This mirrors how
// prepareAuthSpec owns the Render write for a vault-resident
// credential.
type HostAcquireDeps struct {
	// Ctx is the launch context, propagated so the host OAuth flow
	// (browser open, loopback callback wait, token POST) honors
	// cancellation when the user kills the launch.
	Ctx context.Context

	// HTTPClient drives the token-exchange POST against the vendor's
	// token URL. Tests inject httptest-backed clients; production uses
	// a shared client with a sane timeout.
	HTTPClient *http.Client

	// Browser opens the provider's consent URL on the host. The default
	// is oauth.SystemBrowser{}; tests inject a fake Opener so the flow
	// runs headless.
	Browser oauth.Opener

	// CodePrompter reads the authorization code the user copies out of
	// the provider's hosted-callback page and pastes back. It is the
	// seam that keeps the paste on the HOST terminal rather than the
	// container TTY (the original Windows blocker the host flow fixes).
	//
	// promptW is where the acquirer writes the human-facing prompt
	// ("paste the code from your browser:"); the returned string is the
	// trimmed line the user typed. The launcher defaults this to a
	// controlling-terminal line reader (defaultHostCodePrompter); tests
	// inject a scripted prompter so the acquirer stays pure-Go and
	// headless-testable.
	//
	// A nil CodePrompter means the acquirer has no way to read the
	// pasted code: the hosted-callback path must treat that as a
	// non-fatal acquire failure and let the launcher fall back to the
	// in-container login.
	CodePrompter func(ctx context.Context, promptW io.Writer) (string, error)
}

// ErrAuthSpecRenderNil is returned by validateAuthSpec when a
// binding declares no Render function. A binding without Render is
// always a programming error — the launcher would have nothing to
// write.
var ErrAuthSpecRenderNil = errors.New("authspec: binding has nil Render")

// ErrAuthSpecCaptureNil is returned when a FileBinding declares no
// Capture function. Capture is required even when the agent never
// rotates its credential, because Capture is also how first-launch
// vault seeding happens — the in-container login writes the file
// and Capture turns it into a vault entry.
var ErrAuthSpecCaptureNil = errors.New("authspec: file binding has nil Capture")

// ErrAuthSpecEmptyPath is returned when a binding's VaultPath or
// ContainerPath is empty.
var ErrAuthSpecEmptyPath = errors.New("authspec: binding has empty path")

// ErrAuthSpecBadVaultPath is returned when a binding's VaultPath
// does not follow the documented `agents/<name>/<purpose>` scheme.
// The daemon endpoint scopes by name and stores at the canonical
// `agents/<name>/oauth` path; bindings that use a different shape
// would silently misroute today, so validation rejects them up
// front.
var ErrAuthSpecBadVaultPath = errors.New("authspec: vault path does not match agents/<name>/<purpose>")

// vaultPathConforms reports whether a binding's VaultPath follows
// the ADR-0025 namespace scheme. The daemon's per-agent endpoint
// translates `{name}` to `agents/<name>/oauth`; until v1 ships a
// way for the launcher to read arbitrary purposes from the daemon,
// every binding must use that exact suffix or the round-trip
// through the daemon would silently misroute.
func vaultPathConforms(p string) bool {
	p = strings.TrimLeft(p, "/")
	parts := strings.Split(p, "/")
	if len(parts) != 3 {
		return false
	}
	if parts[0] != "agents" {
		return false
	}
	if parts[1] == "" || parts[2] != "oauth" {
		return false
	}
	return true
}

// validateAuthSpec checks that all bindings in a spec have the
// callbacks the launcher requires. Called by the launcher before
// resolving any vault paths so invalid specs surface as a launch
// error rather than a nil-deref deep in the lifecycle.
func validateAuthSpec(spec AuthSpec) error {
	for i, eb := range spec.EnvBindings {
		if eb.VaultPath == "" {
			return fmt.Errorf("%w: EnvBindings[%d]", ErrAuthSpecEmptyPath, i)
		}
		if !vaultPathConforms(eb.VaultPath) {
			return fmt.Errorf("%w: EnvBindings[%d] %q", ErrAuthSpecBadVaultPath, i, eb.VaultPath)
		}
		if eb.Render == nil {
			return fmt.Errorf("%w: EnvBindings[%d] %q", ErrAuthSpecRenderNil, i, eb.VaultPath)
		}
	}
	for i, fb := range spec.FileBindings {
		if fb.VaultPath == "" || fb.ContainerPath == "" {
			return fmt.Errorf("%w: FileBindings[%d]", ErrAuthSpecEmptyPath, i)
		}
		if !vaultPathConforms(fb.VaultPath) {
			return fmt.Errorf("%w: FileBindings[%d] %q", ErrAuthSpecBadVaultPath, i, fb.VaultPath)
		}
		if fb.Render == nil {
			return fmt.Errorf("%w: FileBindings[%d] %q", ErrAuthSpecRenderNil, i, fb.VaultPath)
		}
		if fb.Capture == nil {
			return fmt.Errorf("%w: FileBindings[%d] %q", ErrAuthSpecCaptureNil, i, fb.VaultPath)
		}
	}
	for i, sf := range spec.StaticFiles {
		if sf.ContainerPath == "" {
			return fmt.Errorf("%w: StaticFiles[%d]", ErrAuthSpecEmptyPath, i)
		}
	}
	return nil
}
