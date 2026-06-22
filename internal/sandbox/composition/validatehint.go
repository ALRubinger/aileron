package composition

import (
	"fmt"
	"path"
	"strings"
)

// agentNotFoundMarker is the substring the in-container validation script emits
// when the agent CLI is not on PATH (see internal/sandbox/container/runtime.go
// validationScript). The launch and `sandbox check` call sites match on it to
// detect the CWD-determined-tier mismatch and enrich the error.
const agentNotFoundMarker = "agent command not found in sandbox image:"

// workspaceNotWritableMarker is the substring the in-container validation script
// emits when the writability probe fails (see runtime.go validationScript, the
// exit-3 branch). The bare runtime failure surfaces as an opaque "exit status
// 3"; EnrichValidateError matches this marker to explain the most common cause
// (SELinux denying the confined container process access to the host-labeled
// workspace bind mount) and the remedy.
const workspaceNotWritableMarker = "sandbox workspace is not writable at"

// EnrichValidateError augments a sandbox-image validate failure with tier, CWD,
// and published-image context when the failure is the CWD-determined-tier
// mismatch: a hand-authored .devcontainer/ in workDir selected the devcontainer
// tier (whose project image was built without the requested agent's CLI), while
// a published per-agent image for that agent exists as an alternative.
//
// Discover's tier precedence is unchanged. The authored devcontainer stays
// authoritative; this only makes the failure honest about why the agent was
// absent and what the operator can do. When the failure is not that case
// (different tier, no published image, or a different validate error),
// EnrichValidateError returns err unchanged.
func EnrichValidateError(err error, tier Tier, agent, version, workDir string) error {
	if err == nil {
		return nil
	}
	// The writability failure is tier-independent: any sandbox tier bind-mounts
	// the operator's workspace, and SELinux on the host denies the confined
	// container process access to it regardless of which image was selected.
	// Match and enrich before the devcontainer-specific branch below.
	if strings.Contains(err.Error(), workspaceNotWritableMarker) {
		return enrichWorkspaceNotWritable(err, workDir)
	}
	if tier != TierDevcontainer {
		return err
	}
	if !strings.Contains(err.Error(), agentNotFoundMarker) {
		return err
	}
	if agent == "" || !PublishedAgentExists(agent) {
		return err
	}
	wd := workDir
	if wd == "" {
		wd = "."
	}
	// Use path.Join (forward-slash) rather than filepath.Join: this builds an
	// informational path describing the launch CWD and the devcontainer file for
	// an error message. DefaultDevcontainerPath is a forward-slash literal and
	// the sandbox is a Linux container, so the displayed path must stay POSIX on
	// every host OS rather than picking up Windows backslashes.
	devcontainerPath := path.Join(wd, DefaultDevcontainerPath)
	publishedImage := PublishedAgentImage(agent, version)
	return fmt.Errorf("%w\n"+
		"resolved tier=%s because %s exists in the working directory %s; "+
		"that project image was built without the %q agent CLI on PATH. "+
		"A published per-agent image %s exists as an alternative: "+
		"launch from a directory without a .devcontainer/, or add the %q agent Feature to %s.",
		err, TierDevcontainer, devcontainerPath, wd, agent, publishedImage, agent, devcontainerPath)
}

// enrichWorkspaceNotWritable augments the opaque workspace-writability failure
// with the SELinux root cause and remediation. The bare runtime surface is an
// uninformative "exit status 3"; on a Linux host with SELinux enforcing, the
// confined container process (the image's non-root agent user) is denied access
// to the host-labeled workspace bind mount. Aileron now applies a `:z` relabel
// to the mount automatically, so this message also points operators at the
// manual fallback for hosts where the automatic relabel cannot take effect.
func enrichWorkspaceNotWritable(err error, workDir string) error {
	wd := workDir
	if wd == "" {
		wd = "."
	}
	return fmt.Errorf("%w\n"+
		"the sandbox container could not write to the mounted workspace %s. "+
		"On a Linux host with SELinux in enforcing mode, the host directory's "+
		"SELinux label denies the container's non-root agent user access to the "+
		"bind mount. Aileron applies a shared SELinux relabel (the `:z` mount "+
		"option) automatically when it detects SELinux enforcing; if the failure "+
		"persists, relabel the directory manually with "+
		"`chcon -Rt svirt_sandbox_file_t %s` (or run the host with SELinux "+
		"permissive). On non-SELinux hosts, check that the workspace directory is "+
		"writable by the container's agent user.",
		err, wd, wd)
}
