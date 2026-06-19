package launch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/internal/oauth"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
	"github.com/ALRubinger/aileron/internal/vault"
)

// preLaunchRefreshTimeout caps how long the launcher waits for the
// vendor's token endpoint to respond during a pre-launch refresh. A
// slow or hostile auth.openai.com response must not hang every
// sandbox launch; 15s is conservative (well above the median Codex
// refresh) and short enough that the user sees the failure and can
// recover. Comment kept here rather than at the call site so the
// rationale travels with the constant.
const preLaunchRefreshTimeout = 15 * time.Second

// preLaunchRefreshHTTPClient returns the shared HTTP client used by
// per-binding PreLaunchRefresh hooks. The bare http.DefaultClient
// has no timeout, which would leave launches hung on a slow vendor
// response — RefreshDeps's documented "sane timeout" comes from
// here, not from the caller.
func preLaunchRefreshHTTPClient() *http.Client {
	return &http.Client{Timeout: preLaunchRefreshTimeout}
}

// authSpecDaemon is the subset of *daemonClient the auth-spec
// runtime depends on. Defined as an interface so tests substitute a
// fake without spinning up an httptest server for every scenario.
type authSpecDaemon interface {
	GetAgentCredentials(ctx context.Context, name string) (vault.Secret, error)
	PutAgentCredentials(ctx context.Context, name string, secret vault.Secret) error
}

// authSpecPrep is the output of prepareAuthSpec — everything the
// launcher needs to wire AuthSpec into the sandbox lifecycle without
// the launcher knowing per-agent shape.
type authSpecPrep struct {
	// EnvAdditions merge into the agent's env via composeAgentEnv.
	EnvAdditions map[string]string

	// Mounts append to the sandbox volume list. The launcher passes
	// these alongside proxy bootstrap mounts so the auth-spec dir
	// appears inside the container at the binding's parent path
	// (e.g. /home/agent/.claude/).
	Mounts []sandboxcontainer.Volume

	// CaptureFn fires on clean container exit (and on graceful
	// shutdown). The launcher calls this AFTER launchSandbox returns
	// nil, never on a non-nil error — forcible termination keeps the
	// prior vault entry intact per R15.
	CaptureFn func(ctx context.Context)

	// Cleanup removes the host-side transient directory. Deferred at
	// Launch scope so capture runs first, cleanup second; not folded
	// into launchSandbox's existing defer because the order matters.
	Cleanup func()

	// HasBindings reports whether the prep produced any auth-spec
	// state. False means the agent shipped an empty AuthSpec{} and
	// the launcher should treat the prep as a no-op (Goose, Pi,
	// OpenCode in v1). Useful for skipping the bootstrap-UX status
	// line on agents that have nothing to seed.
	HasBindings bool

	// RenderedAnyCredential reports whether any FileBinding or
	// EnvBinding actually rendered content from the vault. False
	// means every binding was a vault miss (the agent will need to
	// log in inside the container) and the launcher should print
	// the R30 bootstrap UX status line. HasBindings can still be
	// true for an empty-vault launch when a StaticFile lands; the
	// dedicated flag avoids the false negative the
	// `len(EnvAdditions)==0 && len(Mounts)==0` gate produced when
	// any mount existed for a static-only reason.
	RenderedAnyCredential bool
}

// prepareAuthSpec materializes the agent's AuthSpec into the
// container before launch: EnvBindings populate envAdditions, file
// bindings render to a writable per-agent transient directory which
// the launcher bind-mounts at the binding's parent in-container
// path, StaticFiles land in the same directory unconditionally.
//
// The directory is `0700` so OAuth bytes never leak through a
// shared host's process umask. Capture runs on clean container exit
// and snapshots the post-run file bytes back to the daemon's vault;
// the launcher's caller chooses whether to invoke CaptureFn (the
// rule is "Builder.Run returned nil OR graceful shutdown stopped
// the container cleanly").
//
// Capture decides whether to PUT using a presence-aware, three-way
// branch keyed on whether the vault entry was present at render time
// versus at capture time (see ADR-0025's freshness-comparison
// section):
//
//   - absent@render, absent@capture → first-login seed → PUT.
//   - present@render, absent@capture → the operator ran
//     `aileron vault delete` mid-session → honor the delete, skip PUT.
//   - present@render, present@capture → run the binding's Fresher
//     hook; PUT only when the captured envelope is strictly newer.
//
// A FileBinding with a nil Fresher preserves last-writer-wins: the
// present/present branch PUTs unconditionally. This was the behavior
// before the hook existed (concurrent launch races against the same
// agent are not in the v1 ICP, and the refresh token survives the
// race because both writers exchanged the same upstream token).
// Agents that rotate their credential in-container (Claude, Codex)
// supply a Fresher so a stale capture cannot clobber a newer entry.
// chownTransientDir, when non-nil, is invoked once after every
// FileBinding and StaticFile has been written to the transient host
// directory and before the mount list is returned. It receives
// hostRoot and is expected to recursively change ownership of the tree
// to the resolved in-container agent UID. On rootful-Docker Linux the
// host operator's UID owns the transient dir, but the container runs as
// the image's `agent` system user; a writable bind mount preserves host
// ownership, so the agent's mid-session credential rewrite (Claude's
// tmpfile+rename of .credentials.json) fails with EPERM without this
// chown. The launcher supplies a Linux-only closure; on macOS/Windows
// (where the Docker Desktop file-sharing shim translates UIDs) it is
// nil and the chown is skipped. A nil hook is a no-op.
//
// Resolver failures are surfaced as a non-fatal warning and launch
// proceeds (the prior behavior); the hook returns an error only so the
// launcher can decide warn-vs-abort. prepareAuthSpec treats a hook
// error as non-fatal.
func prepareAuthSpec(
	ctx context.Context,
	agentName string,
	spec AuthSpec,
	daemon authSpecDaemon,
	sessionLog *slog.Logger,
	stderr io.Writer,
	chownTransientDir func(dir string) error,
	reclaimTransientDir func(dir string) error,
	hostLoginEnabled bool,
) (authSpecPrep, error) {
	prep := authSpecPrep{
		EnvAdditions: map[string]string{},
		Cleanup:      func() {},
		CaptureFn:    func(context.Context) {},
	}

	if len(spec.EnvBindings) == 0 && len(spec.FileBindings) == 0 && len(spec.StaticFiles) == 0 {
		return prep, nil
	}
	if err := validateAuthSpec(spec); err != nil {
		return prep, fmt.Errorf("auth spec: %w", err)
	}
	prep.HasBindings = true

	// Render env bindings first. They don't touch the host FS and
	// don't need the transient dir, so a Required-but-missing env
	// binding fails fast before any mount setup.
	for i, eb := range spec.EnvBindings {
		secret, err := daemon.GetAgentCredentials(ctx, vaultBindingTail(eb.VaultPath))
		switch {
		case errors.Is(err, ErrAgentCredentialsNotFound):
			if eb.Required {
				return prep, fmt.Errorf("auth spec: env binding %d at %q is required but the vault is empty — seed with `aileron vault put %s`",
					i, eb.VaultPath, eb.VaultPath)
			}
			continue
		case err != nil:
			return prep, fmt.Errorf("auth spec: get env binding %d %q: %w", i, eb.VaultPath, err)
		}
		rendered, err := eb.Render(secret)
		if err != nil {
			return prep, fmt.Errorf("auth spec: render env binding %d %q: %w", i, eb.VaultPath, err)
		}
		for k, v := range rendered {
			prep.EnvAdditions[k] = v
		}
		prep.RenderedAnyCredential = true
	}

	// One transient dir per agent. Group file bindings and static
	// files by their parent container directory so we mount the
	// parent (matches Claude's tmpfile-and-rename dance inside
	// .claude/, and lets Codex co-locate auth.json + config.toml).
	hostRoot, err := os.MkdirTemp("", "aileron-agent-auth-"+agentName+"-*")
	if err != nil {
		return prep, fmt.Errorf("auth spec: create transient dir: %w", err)
	}
	if err := os.Chmod(hostRoot, 0o700); err != nil {
		_ = os.RemoveAll(hostRoot)
		return prep, fmt.Errorf("auth spec: chmod transient dir: %w", err)
	}
	cleanupOnce := false
	cleanup := func() {
		if cleanupOnce {
			return
		}
		cleanupOnce = true
		_ = os.RemoveAll(hostRoot)
	}
	prep.Cleanup = cleanup

	// groups maps a container parent directory (e.g. /home/agent/.claude)
	// to a host-side subdirectory under hostRoot. The launcher mounts
	// each host subdir at the corresponding parent so multiple files
	// under the same parent share one mount. File-mount bindings
	// (MountAsFile = true) still allocate a group dir for their
	// host-side scratch area, but the group dir is NOT mounted into
	// the container — the file mount lands directly at ContainerPath.
	groups := map[string]string{}
	fileMountOnlyGroups := map[string]bool{}
	ensureGroup := func(containerParent string) (string, error) {
		if hostDir, ok := groups[containerParent]; ok {
			return hostDir, nil
		}
		// Mirror the container parent's directory structure inside
		// hostRoot so debugging the transient dir lines up with the
		// in-container view. Strip the leading slash so
		// filepath.Join doesn't reset to absolute.
		rel := containerParent
		for len(rel) > 0 && rel[0] == '/' {
			rel = rel[1:]
		}
		hostDir := filepath.Join(hostRoot, rel)
		if err := os.MkdirAll(hostDir, 0o700); err != nil {
			return "", fmt.Errorf("auth spec: mkdir %s: %w", hostDir, err)
		}
		groups[containerParent] = hostDir
		fileMountOnlyGroups[containerParent] = true
		return hostDir, nil
	}

	// Track file bindings whose host-side files exist after a clean
	// run, plus the metadata captureFn needs to write back. Captured
	// here so CaptureFn closes over the prepared state and doesn't
	// re-traverse the spec.
	type fileCapture struct {
		HostPath  string
		VaultName string // last segment of VaultPath
		Capture   func([]byte) (vault.Secret, error)
		// PresentAtRender records whether the vault entry existed when
		// the launcher rendered this binding. It is the only correct
		// disambiguator between a first-login seed (absent@render) and
		// a deliberate mid-session delete (present@render) when the
		// capture-time GET returns not-found.
		PresentAtRender bool
		// Fresher is the binding's optional freshness hook; nil
		// preserves last-writer-wins. See FileBinding.Fresher.
		Fresher func(captured, current vault.Secret) (bool, error)
	}
	var captureTargets []fileCapture

	// Render file bindings: GET vault entry, run optional pre-launch
	// refresh, Render bytes, write to the host-side subdir under the
	// group dir for the binding's container parent. Track for capture.
	fileMounts := []sandboxcontainer.Volume{}

	// renderAndMount is the shared "render + write + mount +
	// captureTarget" tail for a binding whose Secret is present at
	// render time. It is reached both from the present-vault path and
	// from the host-acquire success path so the two cannot diverge. On
	// any error it runs cleanup and returns a launch-aborting error.
	renderAndMount := func(fb FileBinding, hostDir string, secret vault.Secret) error {
		rendered, renderErr := fb.Render(secret)
		if renderErr != nil {
			cleanup()
			return fmt.Errorf("auth spec: render file binding %q: %w", fb.VaultPath, renderErr)
		}
		hostPath := filepath.Join(hostDir, path.Base(fb.ContainerPath))
		mode := fb.Mode
		if mode == 0 {
			mode = 0o600
		}
		if err := os.WriteFile(hostPath, rendered, mode); err != nil {
			cleanup()
			return fmt.Errorf("auth spec: write %s: %w", hostPath, err)
		}
		prep.RenderedAnyCredential = true
		captureTargets = append(captureTargets, fileCapture{
			HostPath:        hostPath,
			VaultName:       vaultBindingTail(fb.VaultPath),
			Capture:         fb.Capture,
			PresentAtRender: true,
			Fresher:         fb.Fresher,
		})
		// MountAsFile bindings get an individual file mount at
		// ContainerPath. The mount is writable so Capture can read
		// back any in-container update the agent makes; for v1, no
		// agent that uses MountAsFile actually rotates in-container,
		// but writable keeps the API future-proof.
		if fb.MountAsFile {
			fileMounts = append(fileMounts, sandboxcontainer.Volume{
				Source:   hostPath,
				Target:   fb.ContainerPath,
				ReadOnly: false,
			})
		}
		return nil
	}

	for i, fb := range spec.FileBindings {
		containerParent := path.Dir(fb.ContainerPath)
		hostDir, err := ensureGroup(containerParent)
		if err != nil {
			cleanup()
			return prep, err
		}
		// A dir-mount binding cancels the file-mount-only state for
		// the group: the parent gets mounted, all other files in the
		// group ride along.
		if !fb.MountAsFile {
			fileMountOnlyGroups[containerParent] = false
		}
		secret, getErr := daemon.GetAgentCredentials(ctx, vaultBindingTail(fb.VaultPath))
		emptyVault := errors.Is(getErr, ErrAgentCredentialsNotFound)
		switch {
		case emptyVault:
			if fb.Required {
				cleanup()
				return prep, fmt.Errorf("auth spec: file binding %d at %q is required but the vault is empty — seed with `aileron vault put %s`",
					i, fb.VaultPath, fb.VaultPath)
			}
			// Required=false on empty vault. If the binding declares a
			// host acquirer and host-login is enabled, run the host-side
			// OAuth flow, PUT the acquired Secret through the daemon
			// (AE6: vault before render/start), then fall through to the
			// shared render-and-mount tail so the credential is treated
			// exactly as a vault-resident one. The status line in
			// launcher.go is suppressed automatically because the tail
			// sets RenderedAnyCredential = true.
			if fb.HostAcquire != nil && hostLoginEnabled {
				acquired, acqErr := fb.HostAcquire(ctx, HostAcquireDeps{
					Ctx:        ctx,
					HTTPClient: preLaunchRefreshHTTPClient(),
					Browser:    oauth.SystemBrowser{},
				})
				switch {
				case acqErr != nil:
					// Acquire failed or was cancelled. Non-fatal: warn
					// and fall back to the legacy in-container path.
					captureWarn(sessionLog, stderr, agentName, fb.VaultPath,
						fmt.Errorf("host credential acquisition failed: %w; falling back to in-container login", acqErr))
				case len(acquired.Value) == 0:
					// Acquirer returned an empty Secret (e.g. user
					// declined consent). Treat as fallback, no PUT.
					sessionLog.Info("host credential acquisition returned empty secret; falling back to in-container login",
						"agent", agentName, "vault_path", fb.VaultPath)
				default:
					// AE6 ordering: persist to the vault before the
					// container starts. A PUT failure aborts the launch
					// rather than silently dropping a just-acquired
					// credential.
					if err := daemon.PutAgentCredentials(ctx, vaultBindingTail(fb.VaultPath), acquired); err != nil {
						cleanup()
						return prep, fmt.Errorf("auth spec: persist host-acquired credential %q: %w", fb.VaultPath, err)
					}
					if err := renderAndMount(fb, hostDir, acquired); err != nil {
						return prep, err
					}
					continue
				}
			}
			// Legacy in-container path: the agent performs its own login
			// (paste-the-code OAuth fallback for Claude, device-auth for
			// Codex). Capture on clean exit seeds the vault. No file
			// written for this binding, but the group dir still exists
			// for StaticFiles or other file bindings under the same
			// parent.
			captureTargets = append(captureTargets, fileCapture{
				HostPath:        filepath.Join(hostDir, path.Base(fb.ContainerPath)),
				VaultName:       vaultBindingTail(fb.VaultPath),
				Capture:         fb.Capture,
				PresentAtRender: false,
				Fresher:         fb.Fresher,
			})
			continue
		case getErr != nil:
			cleanup()
			return prep, fmt.Errorf("auth spec: get file binding %d %q: %w", i, fb.VaultPath, getErr)
		}

		// Pre-launch refresh runs only when the binding declares one
		// (Codex today). The hook persists the rotated secret to the
		// vault before returning a successful Secret per AE6.
		if fb.PreLaunchRefresh != nil {
			refreshed, refreshErr := fb.PreLaunchRefresh(secret, RefreshDeps{
				Ctx:        ctx,
				HTTPClient: preLaunchRefreshHTTPClient(),
				PutAgentCredentials: func(s vault.Secret) error {
					return daemon.PutAgentCredentials(ctx, vaultBindingTail(fb.VaultPath), s)
				},
			})
			if refreshErr != nil {
				cleanup()
				return prep, fmt.Errorf("auth spec: pre-launch refresh %q: %w", fb.VaultPath, refreshErr)
			}
			secret = refreshed
		}

		if err := renderAndMount(fb, hostDir, secret); err != nil {
			return prep, err
		}
	}

	// Static files land regardless of vault state. They co-locate
	// with file bindings under the same parent so the group dir for
	// /home/agent/.claude/ holds both .credentials.json (file
	// binding) and any future per-agent state.
	//
	// Claude's onboarding stub at /home/agent/.claude.json is a
	// special case: its parent /home/agent/ is also the workspace
	// mount point in v4. We bind-mount /home/agent/.claude.json as
	// a file mount instead of a directory mount to avoid masking
	// the workspace dir.
	staticFileMounts := []sandboxcontainer.Volume{}
	for _, sf := range spec.StaticFiles {
		containerParent := path.Dir(sf.ContainerPath)
		hostDir, err := ensureGroup(containerParent)
		if err != nil {
			cleanup()
			return prep, err
		}
		hostPath := filepath.Join(hostDir, path.Base(sf.ContainerPath))
		mode := sf.Mode
		if mode == 0 {
			mode = 0o644
		}
		if err := os.WriteFile(hostPath, sf.Content, mode); err != nil {
			cleanup()
			return prep, fmt.Errorf("auth spec: write static %s: %w", hostPath, err)
		}
		// Static files at /home/agent/<file> get an individual file
		// mount so the parent dir (the workspace mount) is not masked.
		if containerParent == "/home/agent" {
			staticFileMounts = append(staticFileMounts, sandboxcontainer.Volume{
				Source:   hostPath,
				Target:   sf.ContainerPath,
				ReadOnly: true,
			})
		}
	}

	// Chown the transient tree to the in-container agent UID now that
	// every file is written and before the dir is mounted. On Linux
	// rootful Docker this is the fix that lets the agent rewrite its
	// credentials through the writable bind mount; on other platforms
	// the hook is nil. A resolver/chown failure is non-fatal: warn with
	// a permanent diagnostic naming the UID mismatch and the chown fix,
	// then proceed so a transient lookup hiccup never aborts a launch.
	if chownTransientDir != nil {
		if err := chownTransientDir(hostRoot); err != nil {
			captureWarn(sessionLog, stderr, agentName, hostRoot,
				fmt.Errorf("chown transient auth dir to image agent UID: %w; the agent runs as a non-root system user while the host operator owns this bind-mounted dir (host UID vs resolved agent UID mismatch), so in-container credential rotation may fail with EPERM — the remedy is the chown-to-agent-UID fix on the AuthSpec bind mount", err))
		}
	}

	// Build mount list: one mount per group except for static files
	// directly under /home/agent (those use individual file mounts).
	// Sort parents so the mount order is deterministic for tests.
	parents := make([]string, 0, len(groups))
	for p := range groups {
		parents = append(parents, p)
	}
	sort.Strings(parents)
	for _, p := range parents {
		// Skip the /home/agent group — its only legitimate residents
		// are individual file mounts (Claude's .claude.json today);
		// mounting the dir would mask the workspace.
		if p == "/home/agent" {
			continue
		}
		// Skip groups whose every file binding requested MountAsFile
		// — the individual file mounts added to fileMounts cover them
		// and a parent-dir mount would conflict with any sibling
		// mount the agent's ConfigureMCP installs (Codex's config.toml).
		if fileMountOnlyGroups[p] {
			continue
		}
		prep.Mounts = append(prep.Mounts, sandboxcontainer.Volume{
			Source: groups[p],
			Target: p,
			// Writable: Claude rotates its OAuth credentials mid-
			// session by rewriting .credentials.json via tmpfile +
			// rename, which requires write access to the parent
			// directory. Capture on clean exit reads the updated
			// file back.
			ReadOnly: false,
		})
	}
	prep.Mounts = append(prep.Mounts, fileMounts...)
	prep.Mounts = append(prep.Mounts, staticFileMounts...)

	if len(captureTargets) > 0 {
		prep.CaptureFn = func(captureCtx context.Context) {
			// The agent rotated its credentials in-container as the agent
			// UID; reclaim ownership of the tree to the host operator
			// before reading so the vault PUT can read the rotated file.
			// On rootful Docker Linux the file is otherwise agent-owned
			// and mode 0600, so a host-side read fails with EPERM and the
			// rotation is lost. Non-fatal: warn and still attempt the
			// reads (a same-UID environment needs no reclaim).
			if reclaimTransientDir != nil {
				if err := reclaimTransientDir(hostRoot); err != nil {
					captureWarn(sessionLog, stderr, agentName, hostRoot,
						fmt.Errorf("reclaim transient auth dir ownership to host UID before capture: %w; a credential the agent rotated as its own UID may be unreadable for the vault PUT", err))
				}
			}
			for _, target := range captureTargets {
				bytes, err := os.ReadFile(target.HostPath)
				if err != nil {
					// The file may not exist — agent never wrote it,
					// or the in-container login didn't complete.
					// Skip silently; the prior vault entry (if any)
					// remains usable.
					if os.IsNotExist(err) {
						continue
					}
					captureWarn(sessionLog, stderr, agentName, target.HostPath,
						fmt.Errorf("read for capture: %w", err))
					continue
				}
				if len(bytes) == 0 {
					continue
				}
				captured, err := target.Capture(bytes)
				if err != nil {
					// Schema validation lives inside the binding's
					// Capture func. A parse failure here means the
					// in-container agent left bytes that don't match
					// the documented envelope — log and skip the PUT
					// so the vault isn't overwritten with garbage.
					captureWarn(sessionLog, stderr, agentName, target.HostPath,
						fmt.Errorf("schema-validate captured bytes: %w (skipping vault write so a partial-write or schema-drift session does not clobber the prior entry; inspect %s or re-login)",
							err, target.HostPath))
					continue
				}
				// Decide whether to PUT via the presence-aware,
				// three-way branch (ADR-0025). Read the current vault
				// entry: absent-now plus present-at-render means the
				// operator deleted the entry mid-session and we must
				// honor the delete; absent-now plus absent-at-render is
				// a first-login seed and must PUT.
				current, getErr := daemon.GetAgentCredentials(captureCtx, target.VaultName)
				absentNow := errors.Is(getErr, ErrAgentCredentialsNotFound)
				switch {
				case getErr != nil && !absentNow:
					// A genuine GET failure (or a cancelled context on
					// shutdown). Either way we cannot make the freshness
					// decision safely, so retain the prior entry rather
					// than blind-PUT and risk clobbering a newer one.
					if errors.Is(getErr, context.Canceled) || errors.Is(getErr, context.DeadlineExceeded) {
						captureWarn(sessionLog, stderr, agentName, target.HostPath,
							fmt.Errorf("capture GET cancelled: %w; skipping freshness-gated PUT to avoid clobbering the vault entry on shutdown", getErr))
						continue
					}
					captureWarn(sessionLog, stderr, agentName, target.HostPath,
						fmt.Errorf("read current vault entry for freshness check: %w (skipping PUT so the prior entry is retained; rerun the launch to retry)", getErr))
					continue
				case absentNow && target.PresentAtRender:
					// present@render, absent@capture: the operator ran
					// `aileron vault delete` mid-session. Honor it.
					captureWarn(sessionLog, stderr, agentName, target.HostPath,
						errors.New("vault entry deleted mid-session; honoring the delete and skipping capture so the credential is not silently resurrected"))
					continue
				case absentNow && !target.PresentAtRender:
					// absent@render, absent@capture: first-login seed.
					// PUT unconditionally even when Fresher is non-nil —
					// there is no prior entry to compare against.
				default:
					// present@render, present@capture: gate on Fresher.
					// nil Fresher preserves last-writer-wins.
					if target.Fresher != nil {
						fresher, ferr := target.Fresher(captured, current)
						switch {
						case errors.Is(ferr, ErrCurrentEnvelopeMalformed):
							// The captured envelope parsed but the current
							// vault entry did not. A corrupt entry is unusable
							// (Render rejects it next launch), so overwrite it
							// with the valid capture rather than retaining the
							// corruption. Fall through to the PUT.
							captureWarn(sessionLog, stderr, agentName, target.HostPath,
								fmt.Errorf("current vault entry is malformed: %w; overwriting with the freshly captured credential", ferr))
						case ferr != nil:
							captureWarn(sessionLog, stderr, agentName, target.HostPath,
								fmt.Errorf("freshness comparison failed: %w (skipping PUT so the prior vault entry is retained; inspect %s or re-login)", ferr, target.HostPath))
							continue
						case !fresher:
							// Not a failure: the freshness gate is doing its
							// job. On a re-launch where the agent did not rotate
							// its credential, the captured copy is not newer than
							// the vault entry, so skipping the write is the
							// correct steady-state outcome (and the common case
							// on every clean exit after the first login). Record
							// it in the session log for post-mortems, but do NOT
							// print to stderr — the user should not see a
							// "capture failed" line for normal operation.
							sessionLog.Info("skipping credential capture: not newer than vault entry (freshness gate held)",
								"agent", agentName, "host_path", target.HostPath)
							continue
						}
					}
				}
				if err := daemon.PutAgentCredentials(captureCtx, target.VaultName, captured); err != nil {
					captureWarn(sessionLog, stderr, agentName, target.HostPath,
						fmt.Errorf("persist captured credential: %w (rerun the launch to retry)",
							err))
					continue
				}
				sessionLog.Info("captured agent credentials to vault",
					"agent", agentName, "vault_path", "agents/"+target.VaultName+"/oauth",
					"source", target.HostPath)
			}
		}
	}

	return prep, nil
}

// captureWarn emits a one-line stderr warning per R13 with recovery
// information, and mirrors the same content into the session log so
// the post-mortem has the full context.
func captureWarn(log *slog.Logger, stderr io.Writer, agentName, hostPath string, err error) {
	log.Warn("auth spec capture failed",
		"agent", agentName, "host_path", hostPath, "error", err)
	if stderr != nil {
		fmt.Fprintf(stderr, "[launcher] capture for %s failed: %v\n", agentName, err)
	}
}

// vaultBindingTail extracts the agent identifier from a binding's
// vault path (e.g. `agents/claude/oauth` → `claude`). The daemon's
// per-agent credential endpoints scope by name, so the launcher
// passes only the agent identifier and the daemon reconstructs the
// canonical vault path internally.
//
// validateAuthSpec rejects bindings whose VaultPath does not follow
// the documented `agents/<name>/oauth` scheme, so this helper does
// not need to defend against malformed input on the live path; the
// fallbacks below cover the test paths that probe edge cases.
func vaultBindingTail(vaultPath string) string {
	parts := strings.SplitN(strings.TrimLeft(vaultPath, "/"), "/", 3)
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	default:
		return parts[1]
	}
}
