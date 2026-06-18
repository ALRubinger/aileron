package proxybinding

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
)

// LoadOptions selects the user descriptor layer that overrides the
// built-in defaults. UserPath is optional: an empty path or an absent file
// contributes no entries, so an operator who ships nothing gets exactly the
// built-in profiles.
type LoadOptions struct {
	// UserPath is the per-user descriptor file (e.g. under ~/.aileron),
	// the highest-precedence layer. It overrides built-in entries for the
	// same host. Empty or absent contributes nothing.
	UserPath string
}

// Load merges the two configuration layers (built-in defaults, then user)
// into a single validated, ordered set of entries keyed on host. This
// mirrors the layered policy/config convention used elsewhere in the
// codebase: the later layer overrides the earlier one per host key, so a
// user descriptor can replace a shipped community profile for the same host
// without editing it.
//
// Precedence is strictly built-in < user. Within the merged result, entries
// are ordered deterministically by host so the binding table is
// reproducible across loads.
//
// Every layer is parsed strictly (unknown keys, wrong version, malformed
// YAML, and invalid entries are errors). An invalid layer fails the whole
// load with a clear error and never silently drops entries: a typo in a
// descriptor must not degrade to a partial, surprising binding set. A
// missing user file is not an error (an absent layer is an empty layer);
// only a present-but-unreadable or present-but-invalid file fails.
func Load(opts LoadOptions) ([]Entry, error) {
	merged := make(map[string]Entry)

	builtin, err := parseBuiltinDefaults()
	if err != nil {
		return nil, fmt.Errorf("proxybinding: load built-in defaults: %w", err)
	}
	applyLayer(merged, builtin)

	if opts.UserPath != "" {
		user, err := parseLayerFile(opts.UserPath)
		if err != nil {
			return nil, fmt.Errorf("proxybinding: load user layer %q: %w", opts.UserPath, err)
		}
		applyLayer(merged, user)
	}

	out := make([]Entry, 0, len(merged))
	for _, e := range merged {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}

// applyLayer overlays a layer's entries onto the merged map, last-write-
// wins per host. The map carries the override semantics; ordering is
// re-derived deterministically by Load.
func applyLayer(merged map[string]Entry, layer []Entry) {
	for _, e := range layer {
		merged[e.Host] = e
	}
}

// parseBuiltinDefaults parses every embedded descriptor under defaults/.
// A malformed shipped descriptor is a programming error (it is embedded at
// build time), so it fails the load rather than being skipped.
func parseBuiltinDefaults() ([]Entry, error) {
	dirEntries, err := fs.ReadDir(builtinDefaults, builtinDefaultsDir)
	if err != nil {
		return nil, err
	}
	var out []Entry
	// Sort filenames for a deterministic built-in order before later
	// layers override per host.
	names := make([]string, 0, len(dirEntries))
	for _, de := range dirEntries {
		if de.IsDir() {
			continue
		}
		names = append(names, de.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		data, err := fs.ReadFile(builtinDefaults, builtinDefaultsDir+"/"+name)
		if err != nil {
			return nil, fmt.Errorf("read embedded %s: %w", name, err)
		}
		d, err := Parse(data)
		if err != nil {
			return nil, fmt.Errorf("parse embedded %s: %w", name, err)
		}
		out = append(out, d.Bindings...)
	}
	return out, nil
}

// parseLayerFile reads and parses one on-disk descriptor layer. A missing
// file is not an error: it returns nil entries so an absent layer
// contributes nothing. A present-but-unreadable or present-but-invalid
// file is an error.
func parseLayerFile(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	d, err := Parse(data)
	if err != nil {
		return nil, err
	}
	return d.Bindings, nil
}
