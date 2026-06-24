package composition

// agentRecipe is the canonical, in-repo install recipe for an agent CLI on
// the Aileron Alpine sandbox base. It names the apk prerequisites and the
// global npm package that puts the agent's CLI on PATH.
//
// This table is the single source of truth for the recipe *content* consumed
// by the devcontainer Features under images/sandbox-features/<agent>/, whose
// install.sh scripts express the same recipe. The `aileron sandbox init`
// scaffold no longer emits an inline Dockerfile snippet; it references the
// published Feature directly (see FeatureReference in scaffold.go), so the
// recipe table now feeds only the Features.
//
// features_test.go asserts the Feature install.sh scripts agree with this
// table, so any drift between the two fails CI.
type agentRecipe struct {
	// AptPrereqs are the apk packages installed (via `apk add --no-cache`)
	// before the npm install. Named "prereqs" rather than "apt" deliberately:
	// the Alpine base uses apk, never apt-get.
	Prereqs []string
	// NPMPackage is the global npm package (`npm install -g <pkg>`) that
	// installs the agent's CLI binary onto PATH.
	NPMPackage string
	// Publishable reports whether the agent's CLI license permits Aileron to
	// bake it into a redistributable, publicly-hosted per-agent image. This is
	// distinct from having a recipe: an agent always has a Feature and a
	// local-build path when a recipe exists, but only a publishable agent's
	// image may be pushed to GHCR and pulled build-free (TierPublished). A
	// non-publishable agent (e.g. Claude Code, which is all-rights-reserved with
	// no redistribution grant) keeps its Feature and resolves to a LOCAL Feature
	// build instead of a published image (#1451).
	Publishable bool
}

// agentRecipes maps an agent id to its canonical install recipe. Agents absent
// from this map have no verified Tier 1 recipe and ship no published Feature.
var agentRecipes = map[string]agentRecipe{
	"claude": {
		Prereqs:    []string{"git", "nodejs", "npm", "ripgrep"},
		NPMPackage: "@anthropic-ai/claude-code",
		// @anthropic-ai/claude-code is all-rights-reserved with no
		// redistribution grant, so Aileron may not bake it into a published
		// image. Claude keeps its Feature (local-build path) but is never
		// published (#1451).
		Publishable: false,
	},
	"codex": {
		Prereqs:    []string{"git", "nodejs", "npm", "ripgrep"},
		NPMPackage: "@openai/codex",
		// @openai/codex is Apache-2.0, which grants redistribution, so its
		// per-agent image may be published and pulled build-free.
		Publishable: true,
	},
}

// recipeForAgent returns the canonical recipe for agent and whether one exists.
func recipeForAgent(agent string) (agentRecipe, bool) {
	r, ok := agentRecipes[agent]
	return r, ok
}

// agentPublishable reports whether agent has a recipe AND its CLI license
// permits baking it into a redistributable published image. It is the predicate
// that separates publish-eligibility from recipe-existence (#1451).
func agentPublishable(agent string) bool {
	r, ok := agentRecipes[agent]
	return ok && r.Publishable
}
