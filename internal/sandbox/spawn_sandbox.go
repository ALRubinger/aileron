package sandbox

import "os/exec"

// applyPlatformSandbox is the per-OS hook that hardens a prepared
// os/exec.Cmd before Run. Platform-specific implementations (Linux
// namespaces + Landlock, macOS sandbox-exec, Windows job objects +
// restricted token) live in build-tagged files.
//
// The default v1 implementation in this file is a no-op. The bounded
// env and cwd that the executor sets on cmd are the structural floor;
// the platform sandbox layered on top is what enforces filesystem
// scoping, network denial, and process scoping per ADR-0014. The
// per-platform tightening is a series of follow-ups that override
// this default with cmd.SysProcAttr and friends.
//
// On platforms that ADR-0014 designates as unsupported (BSD, illumos,
// any non-Linux/macOS/Windows OS), the platform-specific override
// returns ErrSpawnUnavailable so hostSpawn translates that into a
// spawn_sandbox_unavailable error.
//
// Unit tests substitute SpawnExecutor wholesale via WithSpawnExecutor
// so the hook is not in the test path. It is the production runtime's
// extension point.
func applyPlatformSandbox(cmd *exec.Cmd, env SpawnEnvelope) error {
	_ = cmd
	_ = env
	return nil
}
