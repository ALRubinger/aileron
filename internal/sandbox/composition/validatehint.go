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
// 3"; EnrichValidateError matches this marker to lead with the one actionable
// remedy (the launch working directory is not writable; run from a directory you
// own) and then append a platform-appropriate deeper diagnostic. The diagnostic
// has distinct causes by platform: a DAC uid mismatch on Linux + Docker (the
// non-root agent user's uid differs from the host workspace owner, so "other"
// carries no write bit on the 0755 directory), a MAC denial on SELinux-enforcing
// hosts, and the C:\WINDOWS\system32 default-CWD trap on Windows (where Docker
// Desktop translates uids and the uid-remap machinery does not apply). The
// enrichment picks the diagnosis from hostOS and whether the `:z` SELinux
// relabel was actually applied (issues #1461 and #1495).
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
func EnrichValidateError(err error, tier Tier, agent, version, workDir, hostOS string, selinuxRelabelActive bool) error {
	if err == nil {
		return nil
	}
	// The writability failure is tier-independent: any sandbox tier bind-mounts
	// the operator's workspace, and the container's non-root agent user can be
	// denied access regardless of which image was selected. Match and enrich
	// before the devcontainer-specific branch below.
	if strings.Contains(err.Error(), workspaceNotWritableMarker) {
		return enrichWorkspaceNotWritable(err, workDir, hostOS, selinuxRelabelActive)
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
// ("exit status 3") with its real root cause and remediation. Every variant
// LEADS with the one actionable sentence that matters to the user — the launch
// working directory is not writable, so run from a directory you own (e.g.
// somewhere under your home) — and only then appends a platform-appropriate
// deeper diagnostic. The headline is the same on every OS; the diagnostic is
// chosen from hostOS and the selinuxRelabelActive flag (from
// container.WorkspaceRelabelActive: Linux + Docker + SELinux enforcing):
//
//   - selinuxRelabelActive (Linux + Docker + SELinux enforcing): Aileron applied
//     the `:z` SELinux relabel to the mount, so a SELinux MAC denial is the
//     plausible remaining cause. Append the SELinux note and the manual `chcon`
//     fallback.
//   - hostOS == "linux" (Docker, no SELinux relabel): the cause is a DAC uid
//     mismatch — the container's non-root agent user has a different numeric uid
//     than the host owner of the workspace, so "other" carries no write bit on
//     the 0755 directory. Append the uid-remap diagnostic, but do NOT claim
//     Aileron applied any relabel (it did not).
//   - Windows: Docker Desktop translates uids at the file-sharing boundary, so
//     the Linux uid-remap machinery does not apply. The common cause is the
//     PowerShell default CWD of C:\WINDOWS\system32, which a normal user cannot
//     write. Append the system32 trap and the `cd` to home remedy.
//   - any other host (macOS, etc.): the file-sharing boundary likewise hides the
//     uid mismatch, so the actionable headline stands alone with no Linux-only
//     uid-remap internals.
//
// The Linux uid-remap detail (aileron-remap-agent-uid aligns the agent uid to
// the workspace owner on Linux + Docker) stays available as the deeper Linux
// diagnostic — a persistent DAC failure points at an image whose entrypoint does
// not run the remap (e.g. a BYO image missing the helper) or a workspace the
// host operator cannot themselves write — but it is no longer the headline, and
// it is never shown on platforms where it does not apply.
func enrichWorkspaceNotWritable(err error, workDir, hostOS string, selinuxRelabelActive bool) error {
	wd := workDir
	if wd == "" {
		wd = "."
	}
	// Headline: the same actionable problem + simplest fix on every platform.
	headline := fmt.Sprintf("the launch working directory %s is not writable by "+
		"you, so the sandbox container cannot write to the mounted workspace. Run "+
		"aileron from a directory you own (for example a project directory under "+
		"your home directory) and try again.", wd)
	switch {
	case selinuxRelabelActive:
		return fmt.Errorf("%w\n%s\n"+
			"Deeper diagnostic: this host runs SELinux in enforcing mode, so the "+
			"host directory's SELinux label can deny the container's non-root agent "+
			"user access to the bind mount. Aileron applied a shared SELinux relabel "+
			"(the `:z` mount option) automatically; if the failure persists on a "+
			"directory you do own, relabel it manually with "+
			"`chcon -Rt svirt_sandbox_file_t %s` (or run the host with SELinux "+
			"permissive).",
			err, headline, wd)
	case hostOS == "linux":
		return fmt.Errorf("%w\n%s\n"+
			"Deeper diagnostic (Linux + Docker): the container's non-root agent user "+
			"does not own the workspace bind mount. Its in-container uid differs "+
			"from the host user that owns %s, so the directory's permissions deny "+
			"the agent write access. Aileron remaps the agent uid to the workspace "+
			"owner at container start (aileron-remap-agent-uid); if the failure "+
			"persists on a directory you do own, the image's entrypoint may not run "+
			"that remap (BYO images must ship the helper on PATH and chain it as "+
			"root before dropping to the agent user).",
			err, headline, wd)
	case hostOS == "windows":
		return fmt.Errorf("%w\n%s\n"+
			"On Windows, PowerShell opens by default in C:\\WINDOWS\\system32, which "+
			"a normal user cannot write. If that is your current directory, `cd` to "+
			"your user home (for example `cd $HOME`) or a project directory under it "+
			"and re-run. (Docker Desktop translates uids at the file-sharing "+
			"boundary, so the Linux uid-remap machinery does not apply here.)",
			err, headline)
	default:
		return fmt.Errorf("%w\n%s", err, headline)
	}
}
