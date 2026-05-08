// Package suite parses Aileron action-suite manifests — the
// "group install" primitive from issue #564. A suite manifest names
// a set of actions that get installed together via
// `aileron action add -f <path>`, sharing connector resolution and
// trust prompts across the whole set.
//
// v1 supports local files only. The parser is closed: unknown
// top-level keys, unknown per-action keys, missing required fields,
// and an empty `[[actions]]` array all surface as errors. The aim
// is to fail loudly when a typo would otherwise silently install a
// different set than the user expected.
package suite

import (
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
)

// Manifest is the parsed form of a suite TOML file.
//
// Example:
//
//	[suite]
//	name = "gmail-and-calendar"
//	description = "Read and draft Gmail; read and create calendar events"
//
//	[[actions]]
//	fqn = "github://owner/repo/actions/list-recent-emails@0.0.6"
//
//	[[actions]]
//	fqn = "github://owner/repo/actions/get-email@0.0.6"
type Manifest struct {
	// Suite is the manifest's metadata block. Required.
	Suite Suite `toml:"suite"`

	// Actions is the ordered list of actions to install. Required and
	// non-empty: a suite with zero actions has no semantic meaning.
	Actions []Action `toml:"actions"`
}

// Suite carries the human-readable identity of the manifest. Both
// fields are required so users (and any future Hub publish path)
// can render a suite without falling back to "untitled".
type Suite struct {
	Name        string `toml:"name"`
	Description string `toml:"description"`
}

// Action is one entry in the suite's install list. Each FQN must
// embed an `@<version>` suffix; v1 doesn't support floating versions
// in suite manifests because the same source could resolve to
// different bytes on different days, breaking reproducibility.
type Action struct {
	// FQN is the fully-qualified `<scheme>://<owner>/<repo>/<subpath>@<version>`
	// reference — same form `aileron action add <FQN>@<version>`
	// accepts on the command line.
	FQN string `toml:"fqn"`
}

// Parse reads suite TOML from data, validates structurally, and
// returns the populated Manifest. `file` is included in error
// messages so users can map the failure back to the source file.
//
// Strict-mode rules:
//   - Unknown top-level keys → error.
//   - Unknown keys inside `[suite]` or each `[[actions]]` → error.
//   - Missing `[suite].name` or `[suite].description` → error.
//   - Empty `[[actions]]` → error.
//   - Each action's `fqn` must be non-empty and contain `@<version>`.
//
// The parser does NOT validate FQN syntax beyond the `@<version>`
// presence check — that's the install pipeline's job (and runs the
// same `cstore.ParseRef` the single-action path uses).
func Parse(file string, data []byte) (*Manifest, error) {
	var m Manifest
	meta, err := toml.Decode(string(data), &m)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", file, err)
	}

	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return nil, fmt.Errorf("parse %q: unknown key(s): %s", file, strings.Join(keys, ", "))
	}

	if m.Suite.Name == "" {
		return nil, fmt.Errorf("parse %q: [suite].name is required", file)
	}
	if m.Suite.Description == "" {
		return nil, fmt.Errorf("parse %q: [suite].description is required", file)
	}
	if len(m.Actions) == 0 {
		return nil, fmt.Errorf("parse %q: at least one [[actions]] entry is required", file)
	}
	for i, a := range m.Actions {
		if a.FQN == "" {
			return nil, fmt.Errorf("parse %q: actions[%d].fqn is required", file, i)
		}
		if !strings.Contains(a.FQN, "@") {
			return nil, fmt.Errorf("parse %q: actions[%d].fqn %q must include @<version>", file, i, a.FQN)
		}
	}
	return &m, nil
}
