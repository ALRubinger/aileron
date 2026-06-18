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
