package composition

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// featuresContext locates the repo-root images/sandbox-features directory by
// walking up from the test working directory for the marker, honoring an
// optional AILERON_SANDBOX_FEATURES_CONTEXT override for parity with the
// AILERON_SANDBOX_BASE_CONTEXT override used by the container runtime.
func featuresContext(t *testing.T) string {
	t.Helper()
	if env := strings.TrimSpace(os.Getenv("AILERON_SANDBOX_FEATURES_CONTEXT")); env != "" {
		return env
	}
	start, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := start
	for {
		candidate := filepath.Join(dir, "images", "sandbox-features")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("images/sandbox-features context not found from %s; set AILERON_SANDBOX_FEATURES_CONTEXT", start)
		}
		dir = parent
	}
}

// featureManifest is the minimal subset of devcontainer-feature.json the
// contract asserts on.
type featureManifest struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

func TestFeatureManifestsAreValid(t *testing.T) {
	root := featuresContext(t)
	for _, agent := range []string{"claude", "codex"} {
		agent := agent
		t.Run(agent, func(t *testing.T) {
			path := filepath.Join(root, agent, "devcontainer-feature.json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			var m featureManifest
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("%s is not valid JSON: %v", path, err)
			}
			if m.ID != agent {
				t.Fatalf("%s id = %q, want %q", path, m.ID, agent)
			}
			if strings.TrimSpace(m.Version) == "" {
				t.Fatalf("%s missing version", path)
			}
			if strings.TrimSpace(m.Name) == "" {
				t.Fatalf("%s missing name", path)
			}
			if strings.TrimSpace(m.Description) == "" {
				t.Fatalf("%s missing description", path)
			}
		})
	}
}

// TestScaffoldFeatureReferenceMatchesManifestVersion guards the drift class that
// motivated #1123: the scaffold once pinned ":1" while the Features published a
// "0.0.1" manifest, so a fresh `sandbox init` + `sandbox build` could not
// resolve the Feature. `devcontainer features publish` emits the tag set
// {major.minor.patch, major.minor, major, latest} from a semver manifest, so the
// scaffold's reference tag is only valid when it is a member of that set. This
// test encodes the Option-B invariant (scaffold pins the major tag, manifest
// stays on the 0.0.x house version) as a contract rather than a literal: a
// future manifest bump to e.g. "0.1.0" still passes because "0" stays in the
// published set, while a scaffold drift to ":1" against a "0.x" manifest or a
// manifest bump that drops the "0" major fails loudly here.
func TestScaffoldFeatureReferenceMatchesManifestVersion(t *testing.T) {
	root := featuresContext(t)
	for _, agent := range []string{"claude", "codex"} {
		agent := agent
		t.Run(agent, func(t *testing.T) {
			path := filepath.Join(root, agent, "devcontainer-feature.json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			var m featureManifest
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("%s is not valid JSON: %v", path, err)
			}

			tags := publishedTagSet(t, m.Version)
			ref := FeatureReference(agent)
			refTag := referenceTag(t, ref)
			if !tags[refTag] {
				t.Fatalf("scaffold reference %q pins tag %q, which is not in the tag set %v published by `devcontainer features publish` for manifest version %q; reconcile internal/sandbox/composition/scaffold.go with %s",
					ref, refTag, sortedKeys(tags), m.Version, path)
			}
		})
	}
}

// publishedTagSet returns the tags `devcontainer features publish` emits for a
// semver manifest version: the full "major.minor.patch", the "major.minor"
// prefix, the "major" prefix, and "latest".
func publishedTagSet(t *testing.T, version string) map[string]bool {
	t.Helper()
	v := strings.TrimSpace(version)
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		t.Fatalf("manifest version %q is not major.minor.patch semver", version)
	}
	for _, p := range parts {
		if p == "" {
			t.Fatalf("manifest version %q has an empty component", version)
		}
	}
	return map[string]bool{
		parts[0]:                  true, // major
		parts[0] + "." + parts[1]: true, // major.minor
		parts[0] + "." + parts[1] + "." + parts[2]: true, // major.minor.patch
		"latest": true,
	}
}

// referenceTag extracts the tag from a Feature reference of the form
// "<registry>/<path>:<tag>". The registry host carries no port in any Aileron
// reference, so the final ":" segment is the tag.
func referenceTag(t *testing.T, ref string) string {
	t.Helper()
	idx := strings.LastIndex(ref, ":")
	if idx < 0 || idx == len(ref)-1 {
		t.Fatalf("Feature reference %q has no tag", ref)
	}
	return ref[idx+1:]
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestFeatureInstallScriptsArePresentAndExecutable(t *testing.T) {
	root := featuresContext(t)
	for _, agent := range []string{"claude", "codex"} {
		agent := agent
		t.Run(agent, func(t *testing.T) {
			path := filepath.Join(root, agent, "install.sh")
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("stat %s: %v", path, err)
			}
			// The executable bit is the load-bearing contract: the devcontainer
			// build runs install.sh, and #965's publish workflow ships exactly
			// what git tracks. Assert git's recorded mode (100755) rather than
			// the filesystem mode, which is platform-dependent — Windows
			// checkouts drop the Unix exec bit, but the committed mode is the
			// real source of truth on every platform.
			assertGitExecutable(t, root, path)
		})
	}
}

// assertGitExecutable fails unless git tracks path with the executable mode
// (100755). root is used as the git working directory so the lookup works
// regardless of the test's cwd.
func assertGitExecutable(t *testing.T, root, path string) {
	t.Helper()
	cmd := exec.Command("git", "ls-files", "--stage", "--", path)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files --stage %s (cwd %s): %v", path, root, err)
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		t.Fatalf("%s is not tracked by git; install.sh must be committed with the executable bit", path)
	}
	// Format: "<mode> <object> <stage>\t<file>"; mode is the first field.
	mode := strings.Fields(line)[0]
	if mode != "100755" {
		t.Fatalf("%s has git mode %s, want 100755 (executable); run `git update-index --chmod=+x` or `chmod +x` before committing", path, mode)
	}
}

func TestFeatureInstallScriptsMatchCanonicalRecipe(t *testing.T) {
	root := featuresContext(t)
	for _, agent := range []string{"claude", "codex"} {
		agent := agent
		t.Run(agent, func(t *testing.T) {
			recipe, ok := recipeForAgent(agent)
			if !ok {
				t.Fatalf("no canonical recipe for agent %q", agent)
			}
			path := filepath.Join(root, agent, "install.sh")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			body := string(raw)

			// The Feature must install via the canonical npm package, and the
			// install line must put that agent's CLI on PATH.
			wantNPM := "npm install -g " + recipe.NPMPackage
			if !strings.Contains(body, wantNPM) {
				t.Fatalf("%s missing canonical npm install %q:\n%s", path, wantNPM, body)
			}

			// Drift guard: the apk prerequisites named in the canonical table
			// must all be installed by the Feature.
			if !strings.Contains(body, "apk add") {
				t.Fatalf("%s must install prerequisites with apk (the Alpine base has no apt-get):\n%s", path, body)
			}
			for _, pkg := range recipe.Prereqs {
				if !strings.Contains(body, pkg) {
					t.Fatalf("%s missing canonical apk prerequisite %q:\n%s", path, pkg, body)
				}
			}

			// The Alpine base uses apk; apt-get must never appear.
			if strings.Contains(body, "apt-get") {
				t.Fatalf("%s uses apt-get; the Alpine base requires apk:\n%s", path, body)
			}

			// No credential reads baked into the Feature.
			for _, secret := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"} {
				if strings.Contains(body, secret) {
					t.Fatalf("%s references credential env var %q; Features must not read or bake credentials:\n%s", path, secret, body)
				}
			}
		})
	}
}
