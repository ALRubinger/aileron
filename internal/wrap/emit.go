package wrap

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/ALRubinger/aileron/internal/cstore"
)

// Emit writes the connector source tree for `s` rooted at `outDir`.
// Files written by default:
//
//	connector/manifest.toml
//	actions/<op>.md  (one per Operations entry; +++-delimited TOML
//	                  frontmatter so the daemon's action loader accepts it)
//
// Shared-forwarder connectors do not ship their own WASM, so the
// emitter no longer produces a Taskfile / release.yml / keys/README.md
// by default. Authors who want to publish a manifest under their own
// FQN (hub-distributed wrap) edit the manifest after emit and follow
// the publisher's guide for signing.
//
// The emitted manifest is verified against cstore.ValidateManifest
// before write. An invalid Spec will cause Emit to fail rather than
// produce an unloadable connector.
//
// `force` controls whether existing files are overwritten. Without
// it, Emit returns an error on the first file that would be replaced.
func Emit(s *Spec, outDir string, force bool) error {
	manifest, manifestBytes, err := buildAndEncode(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	files := buildFiles(s, manifest, manifestBytes)
	for path, content := range files {
		full := filepath.Join(outDir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if !force {
			if _, err := os.Stat(full); err == nil {
				return fmt.Errorf("refusing to overwrite %s (pass force=true to replace)", full)
			}
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// InstallResult is the outcome of [Install]: where the manifest landed
// in the daemon's content-addressed store and which action files were
// written to the user's actions directory.
type InstallResult struct {
	// Hash is the canonical `sha256:<hex>` of the installed manifest
	// per cstore.ForwarderConnectorHash.
	Hash string

	// EntryDir is the daemon-side store directory holding the manifest.
	EntryDir string

	// AlreadyInstalled is true when an entry with the matching hash
	// existed before this call (idempotent reinstall).
	AlreadyInstalled bool

	// ActionFiles lists the absolute paths of the action.md files
	// written under actionsDir. One per declared operation.
	ActionFiles []string
}

// Install writes a Spec directly into the daemon's connector store
// (no source tree on disk) plus one action.md per operation under
// actionsDir. Use this when the connector author wants to immediately
// use the wrapped CLI without going through a publish/install cycle.
//
// `store` is the daemon's [cstore.Store]; the caller wires it from
// the daemon binary (typically via the running daemon's HTTP API
// rather than direct on-disk writes — see cmd/aileron/action_wrap.go).
//
// `actionsDir` is usually action.DefaultDir() (~/.aileron/actions).
// Per-op action files land at `<actionsDir>/<connector-name>-<op>.md`
// so multiple wraps with the same op name (e.g. "list") do not
// collide.
//
// `forwarderBytes` is the daemon-embedded forwarder WASM. Passing it
// in lets the wrap package compute the connector hash without
// importing internal/sandbox/forwarder (which would create a cycle
// for tests that mock the wrap package).
func Install(s *Spec, store *cstore.Store, actionsDir string, forwarderBytes []byte, force bool) (*InstallResult, error) {
	manifest, manifestBytes, err := buildAndEncode(s)
	if err != nil {
		return nil, err
	}
	ref, err := refForSpec(s)
	if err != nil {
		return nil, err
	}
	res, err := store.InstallForwarderManifest(manifestBytes, forwarderBytes, ref)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(actionsDir, 0o755); err != nil {
		return nil, err
	}
	out := &InstallResult{
		Hash:             res.Hash,
		EntryDir:         res.EntryDir,
		AlreadyInstalled: res.AlreadyInstalled,
	}
	for _, sub := range s.Subcommands {
		path := filepath.Join(actionsDir, actionFileName(s, sub))
		if !force {
			if _, statErr := os.Stat(path); statErr == nil {
				return nil, fmt.Errorf("refusing to overwrite %s (pass force=true to replace)", path)
			}
		}
		body := renderActionMD(s, sub, manifest, res.Hash)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return nil, err
		}
		out.ActionFiles = append(out.ActionFiles, path)
	}
	return out, nil
}

// refForSpec extracts the install Ref (FQN + version) from a Spec.
func refForSpec(s *Spec) (cstore.Ref, error) {
	fqn, err := cstore.ParseFQN(s.Connector.Name)
	if err != nil {
		return cstore.Ref{}, fmt.Errorf("parse connector FQN %q: %w", s.Connector.Name, err)
	}
	return cstore.Ref{FQN: fqn, Version: s.Connector.Version}, nil
}

// ActionFileName returns the local filename the action.md gets
// when installed under ~/.aileron/actions/. The connector's leaf
// segment (last path component of the FQN) is the prefix so
// wraps with colliding op names (e.g. two CLIs both with a
// "list" op) do not stomp each other.
//
// Exported so callers outside this package — notably
// `aileron cli add` (#749) — can write action files using the
// same naming convention without re-implementing the rule.
func ActionFileName(s *Spec, sub SubcommandSpec) string {
	return actionFileName(s, sub)
}

func actionFileName(s *Spec, sub SubcommandSpec) string {
	return ActionName(s, sub) + ".md"
}

// ActionName returns the registered action name for a wrapped
// subcommand: `<connector-leaf>-<op>` when the FQN has a leaf
// segment, or the bare op otherwise. The action store keys on
// this value, so namespacing it by connector prevents silent
// collisions between wraps with overlapping op names (e.g. two
// CLIs both exposing an "auth" op).
//
// Action filenames already followed this convention via
// [ActionFileName]; pre-#781 the filename and the action.md's
// `name` field disagreed (filename namespaced, name bare), which
// let two wraps stomp each other in the action store's
// name-keyed map.
func ActionName(s *Spec, sub SubcommandSpec) string {
	prefix := connectorLeaf(s.Connector.Name)
	if prefix == "" {
		return sub.Name
	}
	return prefix + "-" + sub.Name
}

// connectorLeaf returns the last `/`-separated path segment of the
// connector FQN. For `github://acme/gitcrawl` returns `gitcrawl`.
func connectorLeaf(fqn string) string {
	if i := strings.LastIndex(fqn, "/"); i >= 0 {
		return fqn[i+1:]
	}
	return ""
}

// buildAndEncode builds the cstore manifest and serializes it to TOML.
// Returns the parsed manifest, the encoded bytes (suitable as input to
// cstore.ForwarderConnectorHash and Store.InstallForwarderManifest),
// and any validation/encoding error.
func buildAndEncode(s *Spec) (*cstore.Manifest, []byte, error) {
	manifest := BuildManifest(s)
	if err := cstore.ValidateManifest(manifest, "manifest.toml"); err != nil {
		return nil, nil, fmt.Errorf("emitted manifest does not validate: %w", err)
	}
	var buf strings.Builder
	if err := toml.NewEncoder(&buf).Encode(manifest); err != nil {
		return nil, nil, fmt.Errorf("encode manifest: %w", err)
	}
	return manifest, []byte(buf.String()), nil
}

// BuildManifest produces the cstore.Manifest a Spec describes. Pure
// function; suitable for tests that want to inspect the Spec → manifest
// translation without writing files.
func BuildManifest(s *Spec) *cstore.Manifest {
	m := &cstore.Manifest{
		Connector: cstore.ManifestConnector{
			Name:      s.Connector.Name,
			Version:   s.Connector.Version,
			Publisher: s.Connector.Publisher,
			Forwarder: cstore.BuiltinForwarderSpawn,
		},
	}
	if s.Credential != nil {
		m.Capabilities.Credential = &cstore.ManifestCredential{
			Kind:  s.Credential.Kind,
			Scope: s.Credential.Scope,
		}
	}
	spawn := &cstore.ManifestSpawn{
		Programs: []cstore.ManifestSpawnProgram{
			{Path: s.Program.Path, Hash: s.Program.Hash},
		},
		EnvPassthrough:    append([]string(nil), s.EnvPassthrough...),
		CredentialEnvKeys: append([]string(nil), s.CredentialEnvKeys...),
		FSRead:            append([]string(nil), s.FSRead...),
		FSWrite:           append([]string(nil), s.FSWrite...),
		Cwd:               s.Cwd,
		Operations:        map[string]cstore.ManifestSpawnOperation{},
	}
	for _, sub := range s.Subcommands {
		spawn.Operations[sub.Name] = cstore.ManifestSpawnOperation{
			Argv:        sub.Argv,
			Description: sub.Description,
		}
	}
	m.Capabilities.Spawn = spawn
	return m
}

// buildFiles produces the in-memory map of {relpath -> content} the
// emitter writes to disk. Exposed in this form so tests can assert
// on the produced bytes without touching the filesystem.
//
// `manifestBytes` is the encoded manifest TOML; passed in (rather than
// re-encoded) so the bytes hashed into the action.md frontmatter match
// the bytes written to disk exactly.
func buildFiles(s *Spec, m *cstore.Manifest, manifestBytes []byte) map[string]string {
	files := map[string]string{}
	files["connector/manifest.toml"] = string(manifestBytes)
	for _, sub := range s.Subcommands {
		// Use the actions/<op>.md flat layout the daemon's loader expects.
		// Forwarder wraps don't have multi-action sub-directories.
		files["actions/"+sub.Name+".md"] = renderActionMD(s, sub, m, "")
	}
	return files
}

// RenderActionMD is the exported entry point for callers outside
// this package (e.g. `aileron cli add` in #749). Equivalent to
// the internal renderActionMD; kept as a small wrapper so the
// internal signature can evolve without breaking callers.
//
// `connectorHash` may be empty for local-mode connectors
// (LocalStore is name-addressed, not content-addressed); the
// placeholder `sha256:bound-at-install` lands in the action's
// `[[requires.connectors]].hash` field, and the action executor's
// local-FQN router (#749) bypasses the hash lookup entirely.
func RenderActionMD(s *Spec, sub SubcommandSpec, m *cstore.Manifest, connectorHash string) string {
	return renderActionMD(s, sub, m, connectorHash)
}

// renderActionMD produces the action.md body the agent's tool
// surface consumes for one subcommand. The format matches the
// [action.Manifest] schema: TOML frontmatter delimited by `+++` and a
// Markdown body. The runtime's action loader rejects anything else.
//
// `connectorHash` is the canonical `sha256:<hex>` of the connector
// the action references. When non-empty (--install path), the hash
// lands in the action's requires.connectors entry pre-bound. When
// empty (default emit path), a placeholder lands so users can edit
// or have the install pipeline substitute later.
func renderActionMD(s *Spec, sub SubcommandSpec, _ *cstore.Manifest, connectorHash string) string {
	hash := connectorHash
	if hash == "" {
		hash = "sha256:bound-at-install"
	}
	var b strings.Builder
	fmt.Fprintln(&b, frontmatterDelim)
	// Top-level action name is namespaced by connector leaf
	// (`<leaf>-<op>`) so the daemon's action-store name-keyed map
	// doesn't silently overwrite when two wraps share an op (e.g.
	// two CLIs both with `auth`). The action's filename has used
	// this convention since the wrap package's introduction;
	// before #781 the `name` field disagreed with the filename,
	// which is what let the collision through.
	fmt.Fprintf(&b, "name = %q\n", ActionName(s, sub))
	fmt.Fprintf(&b, "version = %q\n", s.Connector.Version)
	fmt.Fprintf(&b, "source = %q\n", actionSource(s, sub))
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "[[requires.connectors]]")
	fmt.Fprintf(&b, "name = %q\n", s.Connector.Name)
	fmt.Fprintf(&b, "version = %q\n", s.Connector.Version)
	fmt.Fprintf(&b, "hash = %q\n", hash)
	// Per ADR-0003 the action's capabilities array names each
	// op the action's execute steps invoke. Wrap-generated
	// action.md files have exactly one execute step (one
	// subcommand per action), so the capabilities array is the
	// single-element list of that op name. The earlier hardcoded
	// ["spawn"] was a mis-encoding — the action-boundary check
	// at executor.go's enforceCapabilityWithSpan compares
	// step.Op against this list, so a wrapped CLI's op never
	// matched and every action returned capability_denied
	// before spawn could fire.
	fmt.Fprintf(&b, "capabilities = [%q]\n", sub.Name)
	fmt.Fprintln(&b)
	if intent := actionIntent(sub); intent != "" {
		fmt.Fprintln(&b, "[match]")
		fmt.Fprintf(&b, "intent = %q\n", intent)
		fmt.Fprintln(&b)
	}
	for _, p := range sub.Params {
		fmt.Fprintln(&b, "[[inputs]]")
		fmt.Fprintf(&b, "name = %q\n", p.Name)
		typ := p.Type
		if typ == "" {
			typ = "string"
		}
		fmt.Fprintf(&b, "type = %q\n", typ)
		desc := p.Description
		if desc == "" {
			desc = p.Name
		}
		fmt.Fprintf(&b, "description = %q\n", desc)
		if !p.Required {
			fmt.Fprintln(&b, "required = false")
		}
		fmt.Fprintln(&b)
	}
	fmt.Fprintln(&b, "[[execute]]")
	fmt.Fprintf(&b, "id = %q\n", sub.Name)
	fmt.Fprintf(&b, "connector = %q\n", s.Connector.Name)
	fmt.Fprintf(&b, "op = %q\n", sub.Name)
	if len(sub.Params) > 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "[execute.inputs]")
		for _, p := range sub.Params {
			fmt.Fprintf(&b, "%s = \"${args.%s}\"\n", p.Name, p.Name)
		}
	}
	fmt.Fprintln(&b, frontmatterDelim)
	fmt.Fprintln(&b)
	if sub.Description != "" {
		fmt.Fprintln(&b, sub.Description)
		fmt.Fprintln(&b)
	}
	fmt.Fprintf(&b, "Wraps `%s`.\n", sub.Argv)
	return b.String()
}

// frontmatterDelim matches internal/action/parse.go: the action loader
// expects `+++` lines on either side of TOML frontmatter.
const frontmatterDelim = "+++"

// actionSource produces the action's provenance source URL.
// Three shapes:
//
//   - `<scheme>://<owner>/<repo>/<op>@<version>` — when the
//     connector FQN already carries a URI scheme. Used by both
//     hub-installed (`hub://...`) and BYOCLI (`local://user/...`)
//     connectors. The action-list path uses the leading scheme
//     to render the ORIGIN badge.
//
//   - `local:<FQN>/<op>@<version>` — fallback for the legacy
//     `aileron action wrap --install` path, which can produce
//     schemeless connector names. Carried as a single `local:`
//     prefix so the action-list path classifies it as a WRAP.
func actionSource(s *Spec, sub SubcommandSpec) string {
	tail := "/" + sub.Name + "@" + s.Connector.Version
	// FQNs with a scheme (`<scheme>://owner/repo`) round-trip as
	// `<source>/<op>@<version>`; the action-list path derives
	// the origin badge from the leading scheme.
	if strings.Contains(s.Connector.Name, "://") {
		return s.Connector.Name + tail
	}
	return "local:" + s.Connector.Name + tail
}

// actionIntent returns the agent-facing intent phrase for a subcommand.
// Falls back to the subcommand name when no description is supplied.
func actionIntent(sub SubcommandSpec) string {
	if sub.Description != "" {
		return sub.Description
	}
	return sub.Name
}

// SortedSubcommands returns the connector's subcommands in
// alphabetical order. Used in tests where iteration order on the
// raw slice is significant; production paths walk the original
// order so users see the YAML they wrote.
func SortedSubcommands(s *Spec) []SubcommandSpec {
	out := append([]SubcommandSpec(nil), s.Subcommands...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
