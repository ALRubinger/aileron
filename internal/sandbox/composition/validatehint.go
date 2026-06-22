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
// 3"; EnrichValidateError matches this marker to explain the real cause and the
// remedy. There are two distinct causes: a DAC uid mismatch (the non-root agent
// user's uid differs from the host workspace owner, so "other" carries no write
// bit on the 0755 directory) and, on SELinux-enforcing hosts, a MAC denial. The
// enrichment picks the right diagnosis from whether the `:z` SELinux relabel was
// actually applied (issue #1461).
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
func EnrichValidateError(err error, tier Tier, agent, version, workDir string, selinuxRelabelActive bool) error {
	if err == nil {
		return nil
	}
	// The writability failure is tier-independent: any sandbox tier bind-mounts
	// the operator's workspace, and the container's non-root agent user can be
	// denied access regardless of which image was selected. Match and enrich
	// before the devcontainer-specific branch below.
	if strings.Contains(err.Error(), workspaceNotWritableMarker) {
		return enrichWorkspaceNotWritable(err, workDir, selinuxRelabelActive)
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
// ("exit status 3") with its real root cause and remediation. There are two
// distinct causes, and the selinuxRelabelActive flag (from
// container.WorkspaceRelabelActive: Linux + Docker + SELinux enforcing)
// disambiguates them:
//
//   - selinuxRelabelActive: Aileron applied the `:z` SELinux relabel to the
//     mount, so a SELinux MAC denial is the plausible remaining cause. Keep the
//     SELinux guidance and the manual `chcon` fallback.
//   - !selinuxRelabelActive: the daemon has no SELinux support, or SELinux is
//     not enforcing, so the `:z` relabel was never emitted. The cause is a DAC
//     uid mismatch — the container's non-root agent user has a different numeric
//     uid than the host owner of the workspace, so "other" carries no write bit
//     on the 0755 directory. Report that mismatch and the remap remediation, and
//     do NOT claim Aileron applied any relabel (it did not).
//
// Aileron's startup remap (aileron-remap-agent-uid) aligns the agent uid to the
// workspace owner on Linux+Docker, so a persistent DAC failure points at an
// image whose entrypoint does not run the remap (e.g. a BYO image missing the
// helper) or a workspace the host operator cannot themselves write.
func enrichWorkspaceNotWritable(err error, workDir string, selinuxRelabelActive bool) error {
	wd := workDir
	if wd == "" {
		wd = "."
	}
	if selinuxRelabelActive {
		return fmt.Errorf("%w\n"+
			"the sandbox container could not write to the mounted workspace %s. "+
			"This host runs SELinux in enforcing mode, so the host directory's "+
			"SELinux label can deny the container's non-root agent user access to "+
			"the bind mount. Aileron applied a shared SELinux relabel (the `:z` "+
			"mount option) automatically; if the failure persists, relabel the "+
			"directory manually with `chcon -Rt svirt_sandbox_file_t %s` (or run "+
			"the host with SELinux permissive).",
			err, wd, wd)
	}
	return fmt.Errorf("%w\n"+
		"the sandbox container could not write to the mounted workspace %s. "+
		"The container's non-root agent user does not own the workspace bind "+
		"mount: its in-container uid differs from the host user that owns %s, so "+
		"the directory's permissions deny the agent write access. Aileron remaps "+
		"the agent uid to the workspace owner at container start "+
		"(aileron-remap-agent-uid) on Linux+Docker; if the failure persists, the "+
		"image's entrypoint may not run that remap (BYO images must ship the "+
		"helper on PATH and chain it as root before dropping to the agent user). "+
		"Confirm the host directory is writable by you, the launching user.",
		err, wd, wd)
}
