//go:build windows

package sandbox

import (
	"fmt"
	"os/exec"
)

// applyPlatformSandbox is the Windows placeholder until the
// job-object + restricted-token + WFP implementation lands
// (tracked under the platform-sandbox umbrella; see
// [ADR-0014]'s Windows section).
//
// Per ADR-0014's "Graceful unavailability" principle, the
// runtime refuses to spawn rather than spawn unconfined.
// When the manifest declares any sandbox parameters the
// function returns [ErrSpawnUnavailable]; legacy callers
// that pass an empty SpawnLimits get the existing no-op
// behavior so pre-sandbox connectors and tests keep
// working.
//
// [ADR-0014]: https://docs.withaileron.ai/adr/0014-spawn-sandbox-technology
func applyPlatformSandbox(cmd *exec.Cmd, env SpawnEnvelope, limits SpawnLimits) error {
	_ = cmd
	_ = env
	if limits.PlatformSandboxRequested() {
		return fmt.Errorf("%w: Windows platform sandbox not yet implemented (see ADR-0014)", ErrSpawnUnavailable)
	}
	return nil
}
