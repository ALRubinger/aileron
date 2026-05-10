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
// Files written:
//
//	connector/manifest.toml
//	actions/<subcommand>/action.md  (one per Subcommands entry)
//	Taskfile.yml
//	.github/workflows/release.yml
//	keys/README.md
//
// The emitted manifest is verified against cstore.ValidateManifest
// before write. An invalid Spec will cause Emit to fail rather than
// produce an unloadable connector.
//
// `force` controls whether existing files are overwritten. Without
// it, Emit returns an error on the first file that would be replaced.
func Emit(s *Spec, outDir string, force bool) error {
	manifest := BuildManifest(s)
	if err := cstore.ValidateManifest(manifest, "manifest.toml"); err != nil {
		return fmt.Errorf("emitted manifest does not validate: %w", err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	files := buildFiles(s, manifest)
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
		EnvPassthrough: append([]string(nil), s.EnvPassthrough...),
		FSRead:         append([]string(nil), s.FSRead...),
		FSWrite:        append([]string(nil), s.FSWrite...),
		Cwd:            s.Cwd,
		Operations:     map[string]cstore.ManifestSpawnOperation{},
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
func buildFiles(s *Spec, m *cstore.Manifest) map[string]string {
	files := map[string]string{}

	// connector/manifest.toml — round-tripped through the BurntSushi
	// encoder so the output is canonical TOML the runtime parses
	// identically. We do not hand-write the TOML because field-order
	// differences across emitter versions would be confusing diffs.
	var manifestBuf strings.Builder
	enc := toml.NewEncoder(&manifestBuf)
	_ = enc.Encode(m)
	files["connector/manifest.toml"] = manifestBuf.String()

	for _, sub := range s.Subcommands {
		files["actions/"+sub.Name+"/action.md"] = renderActionMD(s, sub)
	}

	files["Taskfile.yml"] = renderTaskfile(s)
	files[".github/workflows/release.yml"] = renderReleaseWorkflow(s)
	files["keys/README.md"] = renderKeysReadme(s)
	return files
}

// renderActionMD produces the action.md body the agent's tool
// surface consumes for one subcommand.
func renderActionMD(s *Spec, sub SubcommandSpec) string {
	var b strings.Builder
	fmt.Fprintln(&b, "---")
	fmt.Fprintf(&b, "name: %q\n", sub.Name)
	fmt.Fprintf(&b, "version: %q\n", s.Connector.Version)
	fmt.Fprintf(&b, "connector: %q\n", s.Connector.Name)
	fmt.Fprintln(&b, "capabilities:")
	fmt.Fprintln(&b, "  - spawn")
	if s.Credential != nil {
		fmt.Fprintln(&b, "  - credential")
	}
	if len(sub.Params) > 0 {
		fmt.Fprintln(&b, "params:")
		for _, p := range sub.Params {
			fmt.Fprintf(&b, "  - name: %q\n", p.Name)
			if p.Type != "" {
				fmt.Fprintf(&b, "    type: %q\n", p.Type)
			}
			if p.Description != "" {
				fmt.Fprintf(&b, "    description: %q\n", p.Description)
			}
			if p.Required {
				fmt.Fprintln(&b, "    required: true")
			}
		}
	}
	fmt.Fprintln(&b, "---")
	fmt.Fprintln(&b)
	if sub.Description != "" {
		fmt.Fprintln(&b, sub.Description)
		fmt.Fprintln(&b)
	}
	fmt.Fprintf(&b, "Wraps `%s`.\n", sub.Argv)
	return b.String()
}

// renderTaskfile produces a minimal Taskfile.yml with a build target.
// Connector authors typically extend this with sign / publish tasks
// once #506's reusable composite actions are wired in (deferred per
// the milestone v2 plan).
func renderTaskfile(s *Spec) string {
	var b strings.Builder
	fmt.Fprintln(&b, "version: '3'")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "tasks:")
	fmt.Fprintln(&b, "  build:")
	fmt.Fprintf(&b, "    desc: Build %s connector\n", s.Connector.Name)
	fmt.Fprintln(&b, "    cmds:")
	fmt.Fprintln(&b, "      - go build -o build/connector.wasm ./connector")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "  validate:")
	fmt.Fprintln(&b, "    desc: Validate the connector manifest")
	fmt.Fprintln(&b, "    cmds:")
	fmt.Fprintln(&b, "      - aileron connector validate connector/manifest.toml")
	return b.String()
}

// renderReleaseWorkflow stubs the GitHub Actions release workflow.
// The real workflow uses #506's reusable composite actions; this
// emitter writes a placeholder pointing at the canonical workflow so
// the connector author wires it up after #506 lands.
func renderReleaseWorkflow(s *Spec) string {
	return fmt.Sprintf(`name: release

on:
  push:
    tags:
      - 'v*'

jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      # TODO: replace with the reusable composite action from
      # ALRubinger/aileron-actions once it ships (issue #506):
      #   - uses: ALRubinger/aileron-actions/release-connector@v1
      #     with:
      #       connector_fqn: %q
      - run: echo "release pipeline placeholder for %s"
`, s.Connector.Name, s.Connector.Name)
}

// renderKeysReadme produces the publisher's signing-key onboarding
// README. The connector author commits an ed25519 public key under
// keys/publisher.pub on the source repo's default branch (per
// ADR-0002 §"Publisher identity, signing, and provenance hash").
func renderKeysReadme(s *Spec) string {
	return fmt.Sprintf(`# Signing keys for %s

This directory holds the publisher's signing keys for this connector.

## Setup

1. Generate an ed25519 keypair:

   ` + "```sh" + `
   openssl genpkey -algorithm ed25519 -out keys/publisher.key
   openssl pkey -in keys/publisher.key -pubout -out keys/publisher.pub
   ` + "```" + `

2. Commit ` + "`keys/publisher.pub`" + ` to the source repo's default branch.
   The Aileron install pipeline reads it from there during ` + "`aileron keyring trust`" + `.

3. Keep ` + "`keys/publisher.key`" + ` out of version control. Configure your
   release pipeline to sign release artifacts using a key stored as a
   CI secret.

See [ADR-0002](https://docs.withaileron.ai/adr/0002-connector-model)
for the publisher-identity model.
`, s.Connector.Name)
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
