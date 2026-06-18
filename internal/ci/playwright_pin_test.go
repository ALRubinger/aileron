package ci

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"gopkg.in/yaml.v3"
)

// playwrightPinChecks pairs a CI job whose container bakes a Playwright browser
// image with the pnpm lockfile whose resolved @playwright/test version must
// stay in lockstep with that image tag. When Renovate bumps one without the
// other, the baked browser and the installed @playwright/test drift apart,
// which surfaces as an opaque offline/launch failure in E2E rather than a clear
// "versions don't match" error. This guard turns that silent class of breakage
// into a failing unit test at the source of truth.
var playwrightPinChecks = []struct {
	job      string // job key in .github/workflows/ci.yml
	lockfile string // pnpm lockfile path relative to repo root
}{
	{job: "test-ui", lockfile: filepath.Join("ui", "pnpm-lock.yaml")},
	{job: "test-webapp", lockfile: filepath.Join("webapp", "pnpm-lock.yaml")},
}

// ciWorkflowImages models just enough of ci.yml to read each job's container
// image. Kept separate from ciWorkflow (timeout test) so each guard owns its
// own minimal projection of the file.
type ciWorkflowImages struct {
	Jobs map[string]struct {
		Container struct {
			Image string `yaml:"image"`
		} `yaml:"container"`
	} `yaml:"jobs"`
}

// playwrightImageVersion extracts the semantic version from a Microsoft
// Playwright container image reference such as
// "mcr.microsoft.com/playwright:v1.61.0-noble" -> "1.61.0".
var playwrightImageVersion = regexp.MustCompile(`playwright:v(\d+\.\d+\.\d+)`)

// parsePlaywrightImageTag returns the X.Y.Z version baked into a Playwright
// container image reference.
func parsePlaywrightImageTag(image string) (string, error) {
	m := playwrightImageVersion.FindStringSubmatch(image)
	if m == nil {
		return "", fmt.Errorf("image %q does not look like an mcr.microsoft.com/playwright:vX.Y.Z-* reference", image)
	}
	return m[1], nil
}

// pnpmLockfile models the part of a pnpm-lock.yaml needed to read the resolved
// version of a top-level dependency. The lockfile's `importers` section maps
// each workspace package (the repo root is ".") to its dependencies, and each
// dependency records both the declared `specifier` and the concretely
// `version` pnpm resolved it to. We assert against the resolved version because
// that is the @playwright/test the job actually installs.
type pnpmLockfile struct {
	Importers map[string]struct {
		Dependencies    map[string]pnpmDep `yaml:"dependencies"`
		DevDependencies map[string]pnpmDep `yaml:"devDependencies"`
	} `yaml:"importers"`
}

type pnpmDep struct {
	Specifier string `yaml:"specifier"`
	Version   string `yaml:"version"`
}

// resolvedPlaywrightTestVersion returns the X.Y.Z version pnpm resolved for
// @playwright/test in the root importer of the given lockfile. A pnpm version
// string may carry a peer-dependency suffix (e.g. "1.61.0(foo@2)"); only the
// leading semver is returned.
var leadingSemver = regexp.MustCompile(`^(\d+\.\d+\.\d+)`)

func resolvedPlaywrightTestVersion(lock pnpmLockfile) (string, error) {
	root, ok := lock.Importers["."]
	if !ok {
		return "", fmt.Errorf("lockfile has no root (\".\") importer")
	}
	dep, ok := root.Dependencies["@playwright/test"]
	if !ok {
		dep, ok = root.DevDependencies["@playwright/test"]
	}
	if !ok {
		return "", fmt.Errorf("@playwright/test not found in root importer dependencies or devDependencies")
	}
	m := leadingSemver.FindStringSubmatch(dep.Version)
	if m == nil {
		return "", fmt.Errorf("@playwright/test resolved version %q does not start with an X.Y.Z semver", dep.Version)
	}
	return m[1], nil
}

// TestPlaywrightImageTagMatchesLockfile asserts the contract for #1221: the
// Playwright container image tag pinned in each E2E job must match the
// @playwright/test version resolved in that job's pnpm lockfile. Bumping one
// without the other (the typical Renovate failure mode) makes this test fail
// and names both the job and the divergent versions, instead of letting the
// mismatch surface as an opaque browser-launch error inside E2E.
func TestPlaywrightImageTagMatchesLockfile(t *testing.T) {
	root := findRepoRoot(t)

	workflowPath := filepath.Join(root, ".github", "workflows", "ci.yml")
	data, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", workflowPath, err)
	}

	var wf ciWorkflowImages
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("unmarshal %s: %v", workflowPath, err)
	}

	for _, check := range playwrightPinChecks {
		check := check
		t.Run(check.job, func(t *testing.T) {
			job, ok := wf.Jobs[check.job]
			if !ok {
				t.Fatalf("job %q not found in %s", check.job, workflowPath)
			}
			if job.Container.Image == "" {
				t.Fatalf("job %q has no container.image; expected an mcr.microsoft.com/playwright image", check.job)
			}

			imageVersion, err := parsePlaywrightImageTag(job.Container.Image)
			if err != nil {
				t.Fatalf("job %q: %v", check.job, err)
			}

			lockPath := filepath.Join(root, check.lockfile)
			lockData, err := os.ReadFile(lockPath)
			if err != nil {
				t.Fatalf("read %s: %v", lockPath, err)
			}

			var lock pnpmLockfile
			if err := yaml.Unmarshal(lockData, &lock); err != nil {
				t.Fatalf("unmarshal %s: %v", lockPath, err)
			}

			lockVersion, err := resolvedPlaywrightTestVersion(lock)
			if err != nil {
				t.Fatalf("%s: %v", check.lockfile, err)
			}

			if imageVersion != lockVersion {
				t.Errorf("Playwright version drift in job %q: container image pins v%s but %s resolves @playwright/test %s. Bump the ci.yml image tag and the lockfile in lockstep (JOINT BUMP).", check.job, imageVersion, check.lockfile, lockVersion)
			}
		})
	}
}
