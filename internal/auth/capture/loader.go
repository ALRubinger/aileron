package capture

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
)

// CaptureLoadOptions selects the user descriptor layer that overrides the
// built-in defaults. UserPath is optional: an empty path or an absent file
// contributes no descriptors, so an operator who ships nothing gets
// exactly the built-in tools.
type CaptureLoadOptions struct {
	// UserPath is the per-user descriptor file (e.g. under ~/.aileron),
	// the highest-precedence layer. It overrides built-in descriptors with
	// the same name. Empty or absent contributes nothing.
	UserPath string
}

// LoadCaptureDescriptors merges the two configuration layers (built-in
// defaults, then user) into a single validated set of descriptors keyed on
// name. This mirrors the layered config convention used elsewhere: the
// later layer overrides the earlier one per name, so a user descriptor can
// replace a shipped tool descriptor for the same name without editing it.
//
// Precedence is strictly built-in < user. The returned map is keyed on
// descriptor name. The returned slice is the same set ordered
// deterministically by name so callers that need a stable order (help
// output, tests) get a reproducible result.
//
// Every layer is parsed strictly (unknown keys, wrong version, malformed
// YAML, and invalid descriptors are errors). An invalid layer fails the
// whole load with a clear error and never silently drops descriptors: a
// typo must not degrade to a partial, surprising set. A missing user file
// is not an error (an absent layer is an empty layer); only a
// present-but-unreadable or present-but-invalid file fails. A malformed
// shipped default fails the load: it is embedded at build time, so a bad
// default is a programming error, not a runtime condition.
func LoadCaptureDescriptors(opts CaptureLoadOptions) (map[string]CaptureDescriptor, []CaptureDescriptor, error) {
	merged := make(map[string]CaptureDescriptor)

	builtin, err := parseBuiltinCaptureDefaults()
	if err != nil {
		return nil, nil, fmt.Errorf("capture: load built-in defaults: %w", err)
	}
	applyCaptureLayer(merged, builtin)

	if opts.UserPath != "" {
		user, err := parseCaptureLayerFile(opts.UserPath)
		if err != nil {
			return nil, nil, fmt.Errorf("capture: load user layer %q: %w", opts.UserPath, err)
		}
		applyCaptureLayer(merged, user)
	}

	ordered := make([]CaptureDescriptor, 0, len(merged))
	for _, d := range merged {
		ordered = append(ordered, d)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	return merged, ordered, nil
}

// applyCaptureLayer overlays a layer's descriptors onto the merged map,
// last-write-wins per name.
func applyCaptureLayer(merged map[string]CaptureDescriptor, layer []CaptureDescriptor) {
	for _, d := range layer {
		merged[d.Name] = d
	}
}

// parseBuiltinCaptureDefaults parses every embedded descriptor under
// defaults/. A malformed shipped descriptor fails the load rather than
// being skipped. A duplicate name across two shipped files is also an
// error: each built-in tool must have a unique name within the trusted
// layer.
func parseBuiltinCaptureDefaults() ([]CaptureDescriptor, error) {
	dirEntries, err := fs.ReadDir(builtinCaptureDefaults, builtinCaptureDefaultsDir)
	if err != nil {
		return nil, err
	}
	// Sort filenames for a deterministic built-in order before later
	// layers override per name.
	names := make([]string, 0, len(dirEntries))
	for _, de := range dirEntries {
		if de.IsDir() {
			continue
		}
		names = append(names, de.Name())
	}
	sort.Strings(names)

	var out []CaptureDescriptor
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		data, err := fs.ReadFile(builtinCaptureDefaults, builtinCaptureDefaultsDir+"/"+name)
		if err != nil {
			return nil, fmt.Errorf("read embedded %s: %w", name, err)
		}
		d, err := ParseCaptureDescriptor(data)
		if err != nil {
			return nil, fmt.Errorf("parse embedded %s: %w", name, err)
		}
		if _, dup := seen[d.Name]; dup {
			return nil, fmt.Errorf("duplicate descriptor name %q across embedded defaults", d.Name)
		}
		seen[d.Name] = struct{}{}
		out = append(out, d)
	}
	return out, nil
}

// parseCaptureLayerFile reads and parses one on-disk descriptor layer. A
// missing file is not an error: it returns nil so an absent layer
// contributes nothing. A present-but-unreadable or present-but-invalid
// file is an error.
func parseCaptureLayerFile(path string) ([]CaptureDescriptor, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	d, err := ParseCaptureDescriptor(data)
	if err != nil {
		return nil, err
	}
	return []CaptureDescriptor{d}, nil
}
