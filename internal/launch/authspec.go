package launch

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"

	"github.com/ALRubinger/aileron/internal/vault"
)

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

// validateAuthSpec checks that all bindings in a spec have the
// callbacks the launcher requires. Called by the launcher before
// resolving any vault paths so invalid specs surface as a launch
// error rather than a nil-deref deep in the lifecycle.
func validateAuthSpec(spec AuthSpec) error {
	for i, eb := range spec.EnvBindings {
		if eb.VaultPath == "" {
			return fmt.Errorf("%w: EnvBindings[%d]", ErrAuthSpecEmptyPath, i)
		}
		if eb.Render == nil {
			return fmt.Errorf("%w: EnvBindings[%d] %q", ErrAuthSpecRenderNil, i, eb.VaultPath)
		}
	}
	for i, fb := range spec.FileBindings {
		if fb.VaultPath == "" || fb.ContainerPath == "" {
			return fmt.Errorf("%w: FileBindings[%d]", ErrAuthSpecEmptyPath, i)
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
