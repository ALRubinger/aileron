package launch

import (
	"bytes"
	"context"
	"fmt"
	"os"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"

	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

// agentUIDCacheKey identifies a resolved UID lookup by runtime+image so
// repeated launches against the same image pay the inspect/run cost at
// most once per process.
type agentUIDCacheKey struct {
	runtime string
	image   string
}

var (
	agentUIDCacheMu sync.Mutex
	agentUIDCache   = map[agentUIDCacheKey]int{}
)

// resolveAgentUIDCached wraps resolveAgentUID with a per-process cache
// keyed on (runtime, image). Only successful resolutions are cached; a
// failed lookup is retried on the next launch.
func resolveAgentUIDCached(ctx context.Context, runner sandboxcontainer.Runner, runtime, image string) (int, error) {
	key := agentUIDCacheKey{runtime: runtime, image: image}
	agentUIDCacheMu.Lock()
	if uid, ok := agentUIDCache[key]; ok {
		agentUIDCacheMu.Unlock()
		return uid, nil
	}
	agentUIDCacheMu.Unlock()

	uid, err := resolveAgentUID(ctx, runner, runtime, image)
	if err != nil {
		return 0, err
	}
	agentUIDCacheMu.Lock()
	agentUIDCache[key] = uid
	agentUIDCacheMu.Unlock()
	return uid, nil
}

// newAgentDirChownHook returns the chownTransientDir hook prepareAuthSpec
// invokes after rendering the transient auth directory. The hook only
// performs work on Linux: the macOS/Windows Docker Desktop file-sharing
// shim translates UIDs at the boundary, so chowning the host tree to a
// foreign in-container UID would be both pointless and unnecessary. On
// non-Linux hosts the returned hook is nil and prepareAuthSpec skips the
// chown entirely.
//
// On Linux the hook resolves the image's agent UID (cached per
// runtime+image) and recursively chowns the transient tree to it so the
// agent owns both the mounted parent directory and the rendered
// credential files, enabling the in-container tmpfile+rename rotation
// over the writable bind mount.
//
// The chown runs through the container runtime as root, not via a
// host-side os.Chown. `aileron launch` runs as the unprivileged host
// operator, and changing a file's owner to a different UID requires
// CAP_CHOWN; on rootful Docker a host-side chown to the agent UID fails
// with EPERM. The chown is therefore delegated to a short-lived root
// container that bind-mounts the tree, where the runtime daemon holds
// the privilege. See chownTreeViaRuntime.
//
// Note on capture-on-exit: after the agent rotates a credential
// in-container, the resulting file is owned by the agent UID regardless
// of this hook (the agent writes it). The host-side capture read is
// therefore subject to the same ownership on rootful Docker whether or
// not this chown ran; the hook does not introduce a new capture-read
// failure mode for rotated files. An un-rotated seed file chowned here
// is at worst re-readable only by root/agent, but its capture would
// merely re-PUT identical bytes, so a skipped read is harmless.
func newAgentDirChownHook(ctx context.Context, runner sandboxcontainer.Runner, runtime, image string) func(dir string) error {
	if goruntime.GOOS != "linux" {
		return nil
	}
	// Without a concrete runtime+image the resolver cannot inspect the
	// image; returning nil avoids emitting a spurious per-launch warning
	// on the rare path where sandbox prep left them unset.
	if strings.TrimSpace(runtime) == "" || strings.TrimSpace(image) == "" {
		return nil
	}
	return func(dir string) error {
		uid, err := resolveAgentUIDCached(ctx, runner, runtime, image)
		if err != nil {
			return err
		}
		// Root (UID 0) already owns files created by a rootful host
		// process; the chown would be a no-op, so skip the container run.
		if uid == 0 {
			return nil
		}
		return chownTreeViaRuntime(ctx, runner, runtime, image, dir, uid)
	}
}

// newTransientReclaimHook returns the hook prepareAuthSpec invokes
// before capture-on-exit reads the transient tree. It is the symmetric
// counterpart to newAgentDirChownHook: that hook chowns the tree to the
// agent UID so the in-container agent can rotate its credentials over
// the writable bind mount; this hook chowns the tree back to the host
// operator's UID once the container has exited, so the launcher (running
// as that operator) can read the rotated file for the vault PUT.
//
// Without this reclaim, a rotated credential is left owned by the agent
// UID with mode 0600 on rootful Docker Linux, and the host-side capture
// read fails with EPERM — the rotation is silently dropped, defeating
// the durability the AuthSpec lifecycle exists to provide. Like the
// forward hook it is Linux-only and nil without a concrete runtime+image.
func newTransientReclaimHook(ctx context.Context, runner sandboxcontainer.Runner, runtime, image string) func(dir string) error {
	if goruntime.GOOS != "linux" {
		return nil
	}
	if strings.TrimSpace(runtime) == "" || strings.TrimSpace(image) == "" {
		return nil
	}
	hostUID := os.Getuid()
	return func(dir string) error {
		return chownTreeViaRuntime(ctx, runner, runtime, image, dir, hostUID)
	}
}

// chownTreeViaRuntime recursively changes the owning UID of dir (and
// everything beneath it) to uid by running a short-lived root container
// that bind-mounts dir at /mnt and invokes `chown -R <uid> /mnt`. The
// GID and file modes are left untouched.
//
// This indirection exists because the host launcher is unprivileged:
// os.Chown to a foreign UID needs CAP_CHOWN and fails with EPERM on
// rootful Docker Linux. The container runtime daemon runs as root, so
// the chown is delegated to it. The sandbox image doubles as the helper
// image so no extra pull is needed, and --entrypoint overrides the
// image's entrypoint with chown directly.
func chownTreeViaRuntime(ctx context.Context, runner sandboxcontainer.Runner, runtime, image, dir string, uid int) error {
	if runner == nil {
		return fmt.Errorf("chown via runtime: runner is required")
	}
	if strings.TrimSpace(runtime) == "" {
		return fmt.Errorf("chown via runtime: runtime is required")
	}
	if strings.TrimSpace(image) == "" {
		return fmt.Errorf("chown via runtime: image is required")
	}
	var out bytes.Buffer
	args := []string{
		"run", "--rm",
		"--user", "0",
		"--entrypoint", "chown",
		"--volume", dir + ":/mnt",
		image,
		"-R", strconv.Itoa(uid), "/mnt",
	}
	if err := runner.Run(ctx, runtime, args, &out, &out); err != nil {
		return fmt.Errorf("chown %s to uid %d via %s root container: %w (%s)",
			dir, uid, runtime, err, strings.TrimSpace(out.String()))
	}
	return nil
}

// resolveAgentUID returns the numeric UID of the container image's
// configured USER for the given runtime+image. The UID is derived from
// the image at launch time rather than hardcoded: the sandbox-base
// Containerfile creates the `agent` user with BusyBox `adduser -S`,
// which assigns the UID dynamically, so we cannot assume a fixed value
// (and Tier-1/Tier-2 BYO images may differ again).
//
// Resolution proceeds in two steps:
//
//  1. Inspect the image config's User directive
//     (`<runtime> image inspect --format '{{.Config.User}}'`). An empty
//     result means the image runs as root, so the UID is 0. A numeric
//     spec (`1000` or `1000:1000`) is parsed directly.
//
//  2. A named user (e.g. `agent`) is resolved to its numeric UID by
//     running the image once with `id -u <name>`, which honors the
//     name→UID mapping recorded in the image's /etc/passwd.
//
// The caller is expected to cache the result per (runtime, image) so a
// launch pays the lookup at most once.
func resolveAgentUID(ctx context.Context, runner sandboxcontainer.Runner, runtime, image string) (int, error) {
	if runner == nil {
		return 0, fmt.Errorf("resolve agent uid: runner is required")
	}
	if strings.TrimSpace(runtime) == "" {
		return 0, fmt.Errorf("resolve agent uid: runtime is required")
	}
	if strings.TrimSpace(image) == "" {
		return 0, fmt.Errorf("resolve agent uid: image is required")
	}

	var inspectOut bytes.Buffer
	if err := runner.Run(ctx, runtime,
		[]string{"image", "inspect", "--format", "{{.Config.User}}", image},
		&inspectOut, &inspectOut); err != nil {
		return 0, fmt.Errorf("resolve agent uid: inspect %s: %w (%s)",
			image, err, strings.TrimSpace(inspectOut.String()))
	}

	userSpec := strings.TrimSpace(inspectOut.String())
	// `docker image inspect` prints the literal "<no value>" when the
	// template field is unset on some runtime/version combinations; treat
	// it the same as an empty USER (image runs as root).
	if userSpec == "" || userSpec == "<no value>" {
		return 0, nil
	}

	// User specs may be `user`, `uid`, `user:group`, or `uid:gid`. Only
	// the user component drives file ownership.
	userPart := userSpec
	if idx := strings.IndexByte(userSpec, ':'); idx >= 0 {
		userPart = userSpec[:idx]
	}
	userPart = strings.TrimSpace(userPart)
	if userPart == "" {
		return 0, nil
	}

	// Numeric user component: parse directly, no second container run.
	if uid, err := strconv.Atoi(userPart); err == nil {
		if uid < 0 {
			return 0, fmt.Errorf("resolve agent uid: image %s declares negative uid %d", image, uid)
		}
		return uid, nil
	}

	// Named user: resolve the name→UID mapping by running `id -u <name>`
	// inside the image.
	var idOut bytes.Buffer
	if err := runner.Run(ctx, runtime,
		[]string{"run", "--rm", image, "id", "-u", userPart},
		&idOut, &idOut); err != nil {
		return 0, fmt.Errorf("resolve agent uid: id -u %s in %s: %w (%s)",
			userPart, image, err, strings.TrimSpace(idOut.String()))
	}

	idStr := strings.TrimSpace(idOut.String())
	uid, err := strconv.Atoi(idStr)
	if err != nil {
		return 0, fmt.Errorf("resolve agent uid: parse `id -u %s` output %q from %s: %w",
			userPart, idStr, image, err)
	}
	if uid < 0 {
		return 0, fmt.Errorf("resolve agent uid: image %s resolved user %q to negative uid %d", image, userPart, uid)
	}
	return uid, nil
}
