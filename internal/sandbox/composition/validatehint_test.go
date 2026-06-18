package composition

import (
	"errors"
	"strings"
	"testing"
)

// agentNotFoundErr mimics the validate failure the in-container script produces
// when the agent CLI is not on PATH (see runtime.go validationScript).
func agentNotFoundErr(agent string) error {
	return errors.New("validate sandbox image: agent command not found in sandbox image: " + agent)
}

// TestEnrichValidateErrorDevcontainerMismatch reproduces the CWD-determined-tier
// failure: a .devcontainer/ in workDir selected the devcontainer tier, its
// project image lacks the requested agent CLI, and a published per-agent image
// exists. The enriched error must name the tier, the discovered devcontainer
// path/CWD, and the published image. This fails before the fix (the raw error
// names none of those) and passes after.
func TestEnrichValidateErrorDevcontainerMismatch(t *testing.T) {
	const (
		agent   = "claude"
		version = "1.2.3"
		workDir = "/home/dev/project"
	)
	if !PublishedAgentExists(agent) {
		t.Fatalf("precondition: %q must have a published image", agent)
	}
	base := agentNotFoundErr(agent)
	got := EnrichValidateError(base, TierDevcontainer, agent, version, workDir)
	msg := got.Error()

	if !strings.Contains(msg, string(TierDevcontainer)) {
		t.Errorf("enriched error does not name the tier %q: %s", TierDevcontainer, msg)
	}
	wantPath := workDir + "/" + DefaultDevcontainerPath
	if !strings.Contains(msg, wantPath) {
		t.Errorf("enriched error does not name the devcontainer path %q: %s", wantPath, msg)
	}
	if !strings.Contains(msg, workDir) {
		t.Errorf("enriched error does not name the CWD %q: %s", workDir, msg)
	}
	wantImage := PublishedAgentImage(agent, version)
	if !strings.Contains(msg, wantImage) {
		t.Errorf("enriched error does not name the published image %q: %s", wantImage, msg)
	}
	// The original error must remain wrapped so callers' %w chains still match.
	if !errors.Is(got, base) {
		t.Errorf("enriched error does not wrap the original validate error")
	}
}

func TestEnrichValidateErrorNonDevcontainerTierUnchanged(t *testing.T) {
	base := agentNotFoundErr("claude")
	for _, tier := range []Tier{TierBase, TierPublished, TierBYOImage} {
		got := EnrichValidateError(base, tier, "claude", "1.2.3", "/home/dev/project")
		if got != base {
			t.Errorf("tier %q: error was enriched, want unchanged: %s", tier, got.Error())
		}
	}
}

func TestEnrichValidateErrorUnpublishedAgentUnchanged(t *testing.T) {
	const agent = "no-such-agent"
	if PublishedAgentExists(agent) {
		t.Fatalf("precondition: %q must not have a published image", agent)
	}
	base := agentNotFoundErr(agent)
	got := EnrichValidateError(base, TierDevcontainer, agent, "1.2.3", "/home/dev/project")
	if got != base {
		t.Errorf("unpublished agent: error was enriched, want unchanged: %s", got.Error())
	}
}

func TestEnrichValidateErrorUnrelatedFailureUnchanged(t *testing.T) {
	base := errors.New("validate sandbox image: sandbox workspace is not writable at /workspace")
	got := EnrichValidateError(base, TierDevcontainer, "claude", "1.2.3", "/home/dev/project")
	if got != base {
		t.Errorf("unrelated failure: error was enriched, want unchanged: %s", got.Error())
	}
}

func TestEnrichValidateErrorNilUnchanged(t *testing.T) {
	if got := EnrichValidateError(nil, TierDevcontainer, "claude", "1.2.3", "/home/dev/project"); got != nil {
		t.Errorf("nil error: got %v, want nil", got)
	}
}

func TestEnrichValidateErrorEmptyWorkDirDefaultsToCwd(t *testing.T) {
	base := agentNotFoundErr("claude")
	got := EnrichValidateError(base, TierDevcontainer, "claude", "1.2.3", "")
	// filepath.Join(".", path) cleans to the bare relative path.
	wantPath := DefaultDevcontainerPath
	if !strings.Contains(got.Error(), wantPath) {
		t.Errorf("empty workDir: enriched error does not name %q: %s", wantPath, got.Error())
	}
}
