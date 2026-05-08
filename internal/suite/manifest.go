// Package suite parses Aileron action-suite manifests — the
// "group install" primitive from issue #564. A suite manifest names
// a set of actions that get installed together via
// `aileron action add-suite <source>`, sharing connector resolution
// and trust prompts across the whole set.
//
// The parser is closed: unknown top-level keys, missing required
// fields, and an empty `actions` list all surface as errors. The
// aim is to fail loudly when a typo would otherwise silently install
// a different set than the user expected.
//
// Schema v2 (post-#564 follow-up): top-level keys, no `[suite]`
// table; `actions` is a flat array of strings. Each entry is either
// a path (relative to the suite's repo, version inherited from the
// fetched ref) or a full FQN with explicit `@<version>`. See the
// authoring guide for the definitive reference.
package suite

import (
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/ALRubinger/aileron/internal/cstore"
)

// Manifest is the parsed form of a suite TOML file.
//
// Example (remote, the common case):
//
//	name = "gmail-and-calendar"
//	description = "Read and draft Gmail; read and create calendar events"
//
//	actions = [
//	  "actions/list-recent-emails",
//	  "actions/get-email",
//	]
//
// Example (local, all entries explicit):
//
//	name = "my-bundle"
//	description = "Personal bundle"
//
//	actions = [
//	  "github://acme/conn/actions/foo@0.0.6",
//	  "github://other/repo/actions/bar@1.0.0",
//	]
type Manifest struct {
	// Name is the suite's identifier, shown in the install summary.
	// Required and non-empty.
	Name string `toml:"name"`

	// Description is a one-line human-readable summary. Required
	// and non-empty so published manifests always carry useful
	// metadata; the strict-mode rule is the cheapest way to keep
	// that habit.
	Description string `toml:"description"`

	// Actions is the ordered list of action references to install.
	// Each entry is either a path-form string ("actions/<name>" —
	// version inherited from the suite's fetch ref) or an FQN-form
	// string ("<scheme>://<owner>/<repo>/.../<name>@<version>").
	// Required and non-empty.
	Actions []string `toml:"actions"`
}

// Parse reads suite TOML from data, validates structurally, and
// returns the populated Manifest. `file` is included in error
// messages so users can map the failure back to the source file.
//
// Does NOT classify entries (path-form vs FQN-form) — that's
// `Resolve`'s job. Parse keeps to syntactic validation: required
// fields present, no unknown keys, no empty entries.
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
	if m.Name == "" {
		return nil, fmt.Errorf("parse %q: name is required", file)
	}
	if m.Description == "" {
		return nil, fmt.Errorf("parse %q: description is required", file)
	}
	if len(m.Actions) == 0 {
		return nil, fmt.Errorf("parse %q: at least one entry in actions is required", file)
	}
	for i, raw := range m.Actions {
		if strings.TrimSpace(raw) == "" {
			return nil, fmt.Errorf("parse %q: actions[%d] is empty", file, i)
		}
	}
	return &m, nil
}

// Resolve composes a `cstore.Ref` per `actions` entry, applying
// the path-form vs. FQN-form rule:
//
//   - Path form (no `<scheme>://` prefix): the entry is treated
//     as a subpath relative to suiteFQN. The action installs at
//     suiteVersion. Both must be non-zero, otherwise the entry has
//     no version to inherit (the local-manifest case where path-
//     form entries aren't valid).
//   - FQN form (has `<scheme>://` prefix): parsed via
//     cstore.ParseRef, which enforces strict SemVer on the
//     `@<version>` suffix. The entry MUST include `@<version>`.
//
// The returned slice mirrors the input order. The first error
// encountered aborts; callers wrap it with the source file name.
func Resolve(m *Manifest, suiteFQN cstore.FQN, suiteVersion string) ([]cstore.Ref, error) {
	out := make([]cstore.Ref, 0, len(m.Actions))
	for i, raw := range m.Actions {
		entry := strings.TrimSpace(raw)
		if strings.Contains(entry, "://") {
			ref, err := cstore.ParseRef(entry)
			if err != nil {
				return nil, fmt.Errorf("actions[%d] %q: %w", i, raw, err)
			}
			out = append(out, ref)
			continue
		}
		// Path form. Both the suite's authority and an inheritable
		// version are required.
		if suiteFQN.Scheme == "" || suiteVersion == "" {
			return nil, fmt.Errorf("actions[%d] %q: path-form entries require a remote suite manifest with @<release-tag>; rewrite this entry as a full FQN with explicit @<version>", i, raw)
		}
		path := strings.TrimPrefix(entry, "/")
		if path == "" {
			return nil, fmt.Errorf("actions[%d] %q: empty path", i, raw)
		}
		fqn := cstore.FQN{
			Scheme:  suiteFQN.Scheme,
			Owner:   suiteFQN.Owner,
			Repo:    suiteFQN.Repo,
			Subpath: path,
		}
		out = append(out, cstore.Ref{FQN: fqn, Version: suiteVersion})
	}
	return out, nil
}
