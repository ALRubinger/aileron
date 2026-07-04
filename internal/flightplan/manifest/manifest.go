// Package manifest parses an installable Aileron skill (a SKILL.md
// document: YAML frontmatter plus a Markdown body) and validates the
// Aileron-specific frontmatter block against the normative Flight Plan
// manifest schema.
//
// The agentskills.io SKILL.md format owns the surrounding skill keys
// (name, description, license, ...). Aileron's additive extension is the
// single namespaced top-level `aileron` block, validated here. The block
// is optional: a SKILL.md with no `aileron` block is an instruction-only
// skill and is perfectly valid (the lossless-if-stripped guarantee, see
// ADR-0027). A block that is present but schema-invalid is an error.
//
// This package never reads or carries credential values. The trust
// contract declares the credential kind and placement only (ADR-0005,
// ADR-0019); the resolver matches on names. No secret ever reaches skill
// code through this parser.
package manifest

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrNoFrontmatter is returned by Parse when the document does not open
// with a YAML frontmatter fence.
var ErrNoFrontmatter = errors.New("manifest: no YAML frontmatter found")

// Manifest is the parsed result of a SKILL.md document.
type Manifest struct {
	// Name is the skill's name from the SKILL.md frontmatter (the
	// agentskills.io key). May be empty when the document omits it.
	Name string
	// Description is the skill's description from the frontmatter.
	Description string
	// Body is the Markdown content following the frontmatter fence.
	Body string
	// InstructionOnly reports whether the document carries no `aileron`
	// block. An instruction-only skill has no actions to resolve and the
	// installer short-circuits the requires-resolver for it.
	InstructionOnly bool
	// Aileron is the parsed Aileron block. Zero value when
	// InstructionOnly is true.
	Aileron AileronBlock
}

// AileronBlock is the Aileron-specific frontmatter block.
type AileronBlock struct {
	SchemaVersion string         `yaml:"schemaVersion"`
	Requires      Requires       `yaml:"requires"`
	Environment   *Environment   `yaml:"environment"`
	Inputs        []any          `yaml:"inputs"`
	Outputs       []any          `yaml:"outputs"`
	Steps         []any          `yaml:"steps"`
	Lock          map[string]any `yaml:"lock"`
}

// Environment is the declared execution environment: one container per run.
// Tools names curated-catalog tools (each `<name>@<version>`) freeze composes
// onto the Aileron runner base; Image names a custom base image as the escape
// hatch. The schema requires at least one of the two when the block is
// present; a manifest with no environment block carries a nil *Environment.
type Environment struct {
	Tools []string `yaml:"tools"`
	Image string   `yaml:"image"`
}

// Requires lists the actions the skill calls.
type Requires struct {
	Actions []ActionRequirement `yaml:"actions"`
}

// ActionRequirement is one required action plus its per-action trust
// contract. Only the reference and the trust-contract shape are modeled;
// the trust contract declares credential kind/placement, never a value.
type ActionRequirement struct {
	Ref           string         `yaml:"ref"`
	TrustContract map[string]any `yaml:"trustContract"`
}

// Refs returns the action references declared in requires.actions, in
// declaration order. Instruction-only manifests return nil.
func (m *Manifest) Refs() []string {
	if m.InstructionOnly {
		return nil
	}
	refs := make([]string, 0, len(m.Aileron.Requires.Actions))
	for _, a := range m.Aileron.Requires.Actions {
		refs = append(refs, a.Ref)
	}
	return refs
}

// frontmatterEnvelope decodes only the keys Parse needs from the raw
// frontmatter: the skill name/description and the namespaced aileron block.
// Other agentskills.io keys are intentionally ignored here (they are not
// Aileron's to constrain).
type frontmatterEnvelope struct {
	Name        string        `yaml:"name"`
	Description string        `yaml:"description"`
	Aileron     *AileronBlock `yaml:"aileron"`
}

// Parse decodes a SKILL.md document. It splits the YAML frontmatter from
// the Markdown body, decodes the frontmatter, and — when an `aileron`
// block is present — validates that block against the embedded schema and
// returns field-level errors on failure.
//
// A document with no `aileron` block parses cleanly as InstructionOnly.
// A document with a present-but-invalid block returns a validation error.
func Parse(raw []byte) (*Manifest, error) {
	front, body, ok := splitFrontmatter(raw)
	if !ok {
		return nil, ErrNoFrontmatter
	}

	var env frontmatterEnvelope
	if err := yaml.Unmarshal(front, &env); err != nil {
		return nil, fmt.Errorf("manifest: parse frontmatter YAML: %w", err)
	}

	m := &Manifest{
		Name:        env.Name,
		Description: env.Description,
		Body:        string(body),
	}

	if env.Aileron == nil {
		// No aileron block: instruction-only skill. Lossless-if-stripped:
		// a valid skill the runtime treats as having no actions to resolve.
		m.InstructionOnly = true
		return m, nil
	}

	// The aileron block is present. Validate the full required set against
	// the embedded schema (not just the per-ref regex): aileronBlock
	// requires inputs/outputs; `requires` and its `actions` array are both
	// optional (a tool-only plan declares no connector actions, #1932);
	// each actionRequirement that IS declared requires ref + trustContract.
	if err := validateFrontmatter(front); err != nil {
		return nil, err
	}

	m.Aileron = *env.Aileron
	return m, nil
}

// splitFrontmatter splits a Markdown document into its YAML frontmatter
// block and the remaining body. It expects the document to open with a
// `---` fence and closes on the first standalone `---` line, so a body
// line that merely starts with three dashes (a thematic break, for
// example) is not mistaken for the closing fence. This mirrors the
// splitter proven in internal/flightplan/spec.
func splitFrontmatter(raw []byte) (front, body []byte, ok bool) {
	const fence = "---"
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || lines[0] != fence {
		return nil, nil, false
	}
	for i := 1; i < len(lines); i++ {
		if lines[i] == fence {
			front = []byte(strings.Join(lines[1:i], "\n"))
			body = []byte(strings.Join(lines[i+1:], "\n"))
			return front, body, true
		}
	}
	return nil, nil, false
}
