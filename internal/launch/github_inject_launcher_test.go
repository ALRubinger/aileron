package launch

import (
	"context"
	"testing"

	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
	"github.com/ALRubinger/aileron/internal/sentinel"
	"github.com/ALRubinger/aileron/internal/vault"
)

// Launcher wiring contract (#1149): the launcher merges the GitHub
// injector's EnvAdditions into the composed agentEnv and appends its
// mounts onto the sandbox volume list. These tests exercise that exact
// merge — env merge plus mount append — against a fake daemon, so the
// wiring is verified without standing up the full container launch.
//
// The merge mirrors launcher.go's sandbox-only block:
//
//	for k, v := range ghPrep.EnvAdditions { agentEnv[k] = v }
//	proxyMounts = append(proxyMounts, ghPrep.Mounts...)

func TestLauncherMergesGitHubInject_TokenPresent(t *testing.T) {
	daemon := &fakeUserCredsDaemon{secret: vault.Secret{Value: []byte("ghp_launch")}}

	// Pre-existing agent env (e.g. AuthSpec EnvBindings) with a distinct
	// key set — the GitHub merge must not clobber it.
	agentEnv := map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "agent-oauth"}
	var proxyMounts []sandboxcontainer.Volume

	ghPrep, err := prepareGitHubInject(context.Background(), daemon, githubSentinelSwapBindings(t), nil, nil, nil)
	if err != nil {
		t.Fatalf("prepareGitHubInject: %v", err)
	}
	defer ghPrep.Cleanup()
	for k, v := range ghPrep.EnvAdditions {
		agentEnv[k] = v
	}
	proxyMounts = append(proxyMounts, ghPrep.Mounts...)

	// Sealed model (#1195): the merge appends the secret-free
	// gitconfig mount and sets GH_TOKEN to the NON-SECRET sentinel. The
	// real token never reaches agentEnv.
	if agentEnv["GH_TOKEN"] != sentinel.GitHubTokenSentinel {
		t.Errorf("GH_TOKEN merged into agentEnv = %q, want the sentinel %q", agentEnv["GH_TOKEN"], sentinel.GitHubTokenSentinel)
	}
	if agentEnv["GH_TOKEN"] == "ghp_launch" {
		t.Errorf("GH_TOKEN merged into agentEnv holds the real token; sealed model must not env-inject the secret")
	}
	if agentEnv["CLAUDE_CODE_OAUTH_TOKEN"] != "agent-oauth" {
		t.Errorf("GitHub inject clobbered the AuthSpec env binding: %q", agentEnv["CLAUDE_CODE_OAUTH_TOKEN"])
	}

	// The mount shape is preserved: the static gitconfig is still mounted.
	var found bool
	for _, m := range proxyMounts {
		if m.Target == "/home/agent/.gitconfig" {
			found = true
		}
	}
	if !found {
		t.Errorf("mount list missing /home/agent/.gitconfig; got %+v", proxyMounts)
	}
}

func TestLauncherMergesGitHubInject_NoEntryStillMountsNoopGitconfig(t *testing.T) {
	// Key Decision 4 (#1195): even with no user/github entry the launcher
	// appends the secret-free no-op gitconfig mount (so git-over-HTTPS
	// does not block in the sandbox), but no GH_TOKEN is merged (the
	// sentinel-swap needs a real daemon-side credential).
	daemon := &fakeUserCredsDaemon{err: ErrUserCredentialsNotFound}

	agentEnv := map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "agent-oauth"}
	var proxyMounts []sandboxcontainer.Volume

	ghPrep, err := prepareGitHubInject(context.Background(), daemon, githubSentinelSwapBindings(t), nil, nil, nil)
	if err != nil {
		t.Fatalf("prepareGitHubInject: %v", err)
	}
	defer ghPrep.Cleanup()
	for k, v := range ghPrep.EnvAdditions {
		agentEnv[k] = v
	}
	proxyMounts = append(proxyMounts, ghPrep.Mounts...)

	if _, ok := agentEnv["GH_TOKEN"]; ok {
		t.Errorf("GH_TOKEN injected on no-entry path; want absent")
	}
	var found bool
	for _, m := range proxyMounts {
		if m.Target == "/home/agent/.gitconfig" {
			found = true
			if !m.ReadOnly {
				t.Errorf("no-op gitconfig mount ReadOnly = false, want true")
			}
		}
	}
	if !found {
		t.Errorf("no-op gitconfig mount missing on no-entry path; got %+v", proxyMounts)
	}
	// The unrelated AuthSpec env survives the merge.
	if agentEnv["CLAUDE_CODE_OAUTH_TOKEN"] != "agent-oauth" {
		t.Errorf("no-entry path disturbed existing env: %q", agentEnv["CLAUDE_CODE_OAUTH_TOKEN"])
	}
}
