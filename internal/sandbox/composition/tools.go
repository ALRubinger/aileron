package composition

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// curatedTools is the closed set of curated-catalog tool names an
// `environment.tools` entry may reference. It mirrors the manifest schema's
// closed tool-name pattern (`^(aws-cli|gh)@...` in
// docs/schema/flight-plan-manifest.schema.json) and the Feature recipes
// authored under images/sandbox-features/<name>/. Adding a catalog tool means
// adding its Feature recipe, widening the schema pattern, and adding it here.
var curatedTools = map[string]bool{
	"aws-cli": true,
	"gh":      true,
}

// ResolveToolFeatureRef resolves a declared environment tool reference
// (`<name>@<version>`, for example "aws-cli@2.x") to the published
// devcontainer Feature reference for its curated-catalog recipe (see
// FeatureReference). The name vocabulary is closed to the curated catalog; an
// unknown name or a malformed reference is rejected. The version constraint
// is validated for shape only: the v1 catalog carries one recipe per tool, so
// reproducibility comes from the composed image's digest pin, not from
// resolving the version to a distinct Feature tag.
func ResolveToolFeatureRef(ref string) (string, error) {
	i := strings.LastIndex(ref, "@")
	if i < 0 {
		return "", fmt.Errorf("tool reference %q is not <name>@<version>", ref)
	}
	name, version := ref[:i], ref[i+1:]
	if name == "" || version == "" {
		return "", fmt.Errorf("tool reference %q is not <name>@<version> (both parts must be non-empty)", ref)
	}
	if !curatedTools[name] {
		return "", fmt.Errorf("tool %q is not in the curated catalog (known tools: %s)", name, strings.Join(curatedToolNames(), ", "))
	}
	return FeatureReference(name), nil
}

// curatedToolNames returns the curated tool names sorted for stable error
// messages.
func curatedToolNames() []string {
	names := make([]string, 0, len(curatedTools))
	for name := range curatedTools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ToolsPlan returns the composition Plan that composes the given devcontainer
// Feature references onto base (an image reference, typically digest-pinned
// by the caller so the compose input cannot drift). The plan mirrors the
// synthesized local agent build (#1451): TierDevcontainer with a synthesized
// devcontainer.json materialized into a managed temp workspace folder by the
// container builder, never written into the user's workDir. Image is a
// deterministic local-daemon tag derived from base plus the sorted Feature
// set, so identical compositions share a tag and distinct ones never collide.
func ToolsPlan(base string, featureRefs []string) Plan {
	sorted := sortedRefs(featureRefs)
	features := make(map[string]json.RawMessage, len(sorted))
	for _, ref := range sorted {
		features[ref] = json.RawMessage("{}")
	}
	return Plan{
		Tier:                    TierDevcontainer,
		BaseImage:               base,
		Image:                   LocalToolsImageTag(base, sorted),
		Features:                features,
		SynthesizedDevcontainer: synthesizedToolsDevcontainer(base, sorted),
	}
}

// LocalToolsImageTag returns the local-only image tag for an image composed
// from base plus the given Feature references. The tag is deterministic per
// (base, sorted Feature set): a short content hash keys it, so the `auto`
// build policy can skip a rebuild for an identical composition while distinct
// compositions get distinct tags. The "aileron/" prefix (no registry host)
// keeps it a local-daemon tag, mirroring LocalAgentImageTag's convention; it
// is never pushed to a registry.
func LocalToolsImageTag(base string, featureRefs []string) string {
	sorted := sortedRefs(featureRefs)
	h := sha256.New()
	h.Write([]byte(base))
	for _, ref := range sorted {
		h.Write([]byte{0})
		h.Write([]byte(ref))
	}
	return "aileron/sandbox-tools:" + hex.EncodeToString(h.Sum(nil))[:16]
}

// sortedRefs returns a sorted copy of refs so plan content and tags are
// deterministic regardless of declaration order.
func sortedRefs(refs []string) []string {
	sorted := append([]string(nil), refs...)
	sort.Strings(sorted)
	return sorted
}

// synthesizedToolsDevcontainer renders the devcontainer.json that composes
// the given Feature references onto base. It is the multi-Feature sibling of
// synthesizedAgentDevcontainer: a build-only artifact with no customizations
// block, materialized into a managed temp workspace folder by the container
// builder (#1451). Feature order is the caller's (sorted) order, so the
// rendered bytes are deterministic for a given composition.
func synthesizedToolsDevcontainer(base string, sortedFeatureRefs []string) string {
	var sb strings.Builder
	sb.WriteString("{\n  \"name\": \"Aileron sandbox\",\n")
	fmt.Fprintf(&sb, "  \"image\": %q,\n", base)
	sb.WriteString("  \"features\": {\n")
	for i, ref := range sortedFeatureRefs {
		sep := ","
		if i == len(sortedFeatureRefs)-1 {
			sep = ""
		}
		fmt.Fprintf(&sb, "    %q: {}%s\n", ref, sep)
	}
	sb.WriteString("  }\n}\n")
	return sb.String()
}
