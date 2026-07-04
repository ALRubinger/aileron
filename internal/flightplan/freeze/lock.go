package freeze

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ImagePin pairs a pre-freeze image reference with the content-addressed
// digest freeze resolved it to. The schema `$defs.lock.resolvedImages[]` item
// admits {ref, digest, localTag?}: the plan runs in one container, so freeze
// emits at most one pin and no per-pin step linkage.
type ImagePin struct {
	Ref    string `yaml:"ref" json:"ref"`
	Digest string `yaml:"digest" json:"digest"`
	// LocalTag is the bootable local-daemon tag for a composed-tools pin
	// (`aileron/sandbox-tools:<hex>`), the identity the composed image carries
	// in the local daemon. It is set only on the composed-tools pin: an
	// image-only or custom-base pin leaves it empty and boots `ref@digest`.
	// omitempty keeps those non-composed locks byte-identical to the
	// pre-localTag format. Sealed inside the signed lock (covered by the
	// content hash and signature), so it cannot be re-supplied at launch.
	LocalTag string `yaml:"localTag,omitempty" json:"localTag,omitempty"`
}

// StepReach is the sealed network reach for one tool step: the `hosts` from
// its declared trust contract, resolved at freeze time. It mirrors a schema
// `$defs.lock.stepTrust` entry exactly. Hosts only: the full contract stays
// in the signed frontmatter bytes.
type StepReach struct {
	Hosts []string `yaml:"hosts" json:"hosts"`
}

// Lockfile is the resolved lock-and-digest record freeze produces. Its
// field names and shape match the schema `$defs.lock` so the same data
// round-trips into the manifest `lock` block and the standalone
// `aileron.lock` artifact.
type Lockfile struct {
	// ResolvedImages are the resolved image digest pins. Empty for an
	// instruction-only or no-execution-environment skill.
	ResolvedImages []ImagePin `yaml:"resolvedImages,omitempty" json:"resolvedImages,omitempty"`
	// ResolvedCapabilitySet is the capability set freeze pinned: the declared
	// environment tools, verbatim. Empty when no tools are declared.
	ResolvedCapabilitySet []string `yaml:"resolvedCapabilitySet,omitempty" json:"resolvedCapabilitySet,omitempty"`
	// StepTrust is the step-keyed sealed trust section: for each tool step
	// that declares a trust contract, the resolved network reach freeze
	// sealed from it, keyed by step id. Covered by the content hash and
	// signature, so the reach cannot be re-supplied at launch. Nil when no
	// tool step declares a contract (omitempty keeps those locks
	// byte-identical to the pre-stepTrust format). yaml.v3 marshals map keys
	// sorted, so the emitted section is byte-deterministic across freezes.
	StepTrust map[string]StepReach `yaml:"stepTrust,omitempty" json:"stepTrust,omitempty"`
	// Publisher is the connector-style publisher authority the frozen plan is
	// attributed to (`github://owner/repo` or bare `github://owner`), recorded
	// from `freeze --publisher`. It is NOT cleared by withoutContentHash, so it
	// participates in the content hash and is covered by the signature: the
	// declared publisher cannot be re-supplied at launch. Launch resolves the
	// plan's signing key against the keyring for this authority and refuses when
	// the publisher is not trusted. Empty for a publisher-less plan (omitempty
	// keeps those locks byte-identical to the pre-publisher format), which
	// carries no launch-time publisher-trust gate.
	Publisher string `yaml:"publisher,omitempty" json:"publisher,omitempty"`
	// ContentHash identifies the exact frozen manifest bytes
	// (`sha256:<hex>`). It is computed last and excluded from the bytes it
	// hashes, so a Lockfile passed to the content hasher must have it empty.
	ContentHash string `yaml:"contentHash,omitempty" json:"contentHash,omitempty"`
	// Version is the human-facing semver label for this frozen version.
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
}

// withoutContentHash returns a copy of the lockfile with ContentHash
// cleared. The content hash cannot reference its own value, so the bytes it
// is taken over must omit it (see content.go).
func (l Lockfile) withoutContentHash() Lockfile {
	l.ContentHash = ""
	return l
}

// MarshalLockfile renders the lockfile as the standalone `aileron.lock`
// artifact: stable YAML with the schema field names.
func MarshalLockfile(l Lockfile) ([]byte, error) {
	out, err := yaml.Marshal(l)
	if err != nil {
		return nil, fmt.Errorf("freeze: marshal lockfile: %w", err)
	}
	return out, nil
}

// lockNode renders a Lockfile as a yaml.Node mapping suitable for splicing
// under the `aileron` block. Marshaling through a typed value and decoding
// back into a Node keeps the omitempty rules and field order consistent
// with MarshalLockfile.
func lockNode(l Lockfile) (*yaml.Node, error) {
	raw, err := yaml.Marshal(l)
	if err != nil {
		return nil, fmt.Errorf("freeze: marshal lock block: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("freeze: decode lock block: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, fmt.Errorf("freeze: unexpected lock block shape")
	}
	return doc.Content[0], nil
}

// injectLock rewrites raw SKILL.md bytes so the `aileron` block carries the
// given lock. It splits the frontmatter, decodes it as a YAML node tree
// (preserving every sibling key and its order), sets or replaces
// `aileron.lock`, and re-emits the document. The result re-parses cleanly
// through manifest.Parse.
//
// Freeze canonicalizes line endings to LF (the same normalization
// manifest.Parse applies when splitting frontmatter). The frozen output is
// therefore LF-canonical regardless of the input's line endings, which
// keeps the content hash and signature stable: a CRLF SKILL.md and its LF
// twin freeze to byte-identical artifacts. The Markdown body is preserved
// verbatim aside from this LF canonicalization.
//
// injectLock is the freeze-time write primitive: there is no manifest
// re-emit path in the manifest package, so freeze injects the lock into the
// raw frontmatter here.
// injectLockMaybe injects the lock when the manifest has an aileron block,
// and returns the LF-canonicalized manifest unchanged when it is
// instruction-only (no aileron block to carry a lock). An instruction-only
// Flight Plan still gets a content hash and signature: the lock data lives
// in the standalone lockfile, which is part of the hashed canonical bytes.
func injectLockMaybe(raw []byte, l Lockfile, instructionOnly bool) ([]byte, error) {
	if instructionOnly {
		return canonicalizeNewlines(raw), nil
	}
	return injectLock(raw, l)
}

// canonicalizeNewlines normalizes CRLF to LF so an instruction-only frozen
// manifest follows the same freeze-wide newline contract as the
// lock-injection path.
func canonicalizeNewlines(raw []byte) []byte {
	return []byte(strings.ReplaceAll(string(raw), "\r\n", "\n"))
}

func injectLock(raw []byte, l Lockfile) ([]byte, error) {
	front, body, fence, ok := splitFrontmatter(raw)
	if !ok {
		return nil, fmt.Errorf("freeze: no YAML frontmatter to inject lock into")
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(front, &doc); err != nil {
		return nil, fmt.Errorf("freeze: parse frontmatter for lock injection: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, fmt.Errorf("freeze: frontmatter is not a YAML mapping")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("freeze: frontmatter root is not a mapping")
	}

	aileron := mappingValue(root, "aileron")
	if aileron == nil {
		return nil, fmt.Errorf("freeze: manifest has no aileron block to lock")
	}
	if aileron.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("freeze: aileron block is not a mapping")
	}

	ln, err := lockNode(l)
	if err != nil {
		return nil, err
	}
	setMappingValue(aileron, "lock", ln)

	frontOut, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("freeze: re-emit frontmatter: %w", err)
	}
	frontOut = []byte(strings.TrimRight(string(frontOut), "\n"))

	var b strings.Builder
	b.WriteString(fence)
	b.WriteByte('\n')
	b.Write(frontOut)
	b.WriteByte('\n')
	b.WriteString(fence)
	b.WriteByte('\n')
	b.WriteString(body)
	return []byte(b.String()), nil
}

// mappingValue returns the value node for key in a mapping node, or nil.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// setMappingValue sets key to val in a mapping node, replacing an existing
// entry or appending a new key/value pair at the end.
func setMappingValue(m *yaml.Node, key string, val *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = val
			return
		}
	}
	k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	m.Content = append(m.Content, k, val)
}

// splitFrontmatter splits a SKILL.md document into its YAML frontmatter and
// the remaining body, returning the fence string used. It mirrors the
// manifest package splitter: it opens on a `---` fence and closes on the
// first standalone `---` line so a body thematic break is never mistaken
// for the closing fence. Line endings are canonicalized to LF (see
// injectLock): this is the freeze-wide newline contract that keeps frozen
// output stable across CRLF and LF inputs.
func splitFrontmatter(raw []byte) (front []byte, body, fence string, ok bool) {
	const f = "---"
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || lines[0] != f {
		return nil, "", "", false
	}
	for i := 1; i < len(lines); i++ {
		if lines[i] == f {
			front = []byte(strings.Join(lines[1:i], "\n"))
			body = strings.Join(lines[i+1:], "\n")
			return front, body, f, true
		}
	}
	return nil, "", "", false
}
