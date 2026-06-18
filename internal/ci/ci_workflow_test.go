package ci

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// githubActionsDefaultTimeoutMinutes is the implicit per-job ceiling GitHub
// Actions applies when a job declares no timeout-minutes. The whole point of
// this guard is to ensure no job inherits it, so any declared value must be
// strictly below it.
const githubActionsDefaultTimeoutMinutes = 360

// ciWorkflow models just enough of .github/workflows/ci.yml to inspect each
// job's timeout-minutes. yaml.Node is used for the value so a missing key is
// distinguishable from a present-but-zero one, and so we can report the
// offending job by name.
type ciWorkflow struct {
	Jobs map[string]struct {
		TimeoutMinutes *int `yaml:"timeout-minutes"`
	} `yaml:"jobs"`
}

// findRepoRoot walks up from the test's working directory until it finds a
// directory containing .github/workflows/ci.yml. This makes the test robust to
// being invoked from the internal module root (cwd = internal, as the CI `rest`
// shard does) or from the repo root (go test ./internal/ci/...).
func findRepoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	for {
		candidate := filepath.Join(dir, ".github", "workflows", "ci.yml")
		if _, statErr := os.Stat(candidate); statErr == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate .github/workflows/ci.yml walking up from working directory")
		}
		dir = parent
	}
}

// TestEveryCIJobHasTimeoutMinutes asserts the contract for #1216: every job in
// .github/workflows/ci.yml declares a job-level timeout-minutes so a stalled
// step fails fast instead of inheriting the 360-minute GitHub Actions default.
//
// The job set is derived from the file at test time — it is not hard-coded — so
// jobs added later are automatically held to the same contract. Deleting any
// job's timeout-minutes makes this test fail and names the offending job.
func TestEveryCIJobHasTimeoutMinutes(t *testing.T) {
	root := findRepoRoot(t)
	path := filepath.Join(root, ".github", "workflows", "ci.yml")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var wf ciWorkflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}

	if len(wf.Jobs) == 0 {
		t.Fatalf("no jobs found in %s; expected ci.yml to define at least one job", path)
	}

	for name, job := range wf.Jobs {
		switch {
		case job.TimeoutMinutes == nil:
			t.Errorf("job %q is missing job-level timeout-minutes; a stalled step would inherit the %d-minute GitHub Actions default", name, githubActionsDefaultTimeoutMinutes)
		case *job.TimeoutMinutes <= 0:
			t.Errorf("job %q has non-positive timeout-minutes %d; expected a positive integer", name, *job.TimeoutMinutes)
		case *job.TimeoutMinutes >= githubActionsDefaultTimeoutMinutes:
			t.Errorf("job %q has timeout-minutes %d which meets or exceeds the %d-minute GitHub Actions default; set a tighter bound so the job fails fast", name, *job.TimeoutMinutes, githubActionsDefaultTimeoutMinutes)
		}
	}
}
