package capture

import (
	"fmt"
	"sort"
)

// Registry resolves a capture descriptor by name. It is a thin,
// name-keyed wrapper over the loaded, layer-merged descriptor set. The
// registry settles acquisition by tool name (e.g. "gh"); there is no
// per-tool flag — the name is the only selector.
type Registry struct {
	byName map[string]CaptureDescriptor
}

// NewRegistry builds a Registry from the merged built-in + user descriptor
// layers selected by opts. A load error (malformed shipped default,
// present-but-invalid user file) is surfaced rather than degrading to a
// partial registry.
func NewRegistry(opts CaptureLoadOptions) (*Registry, error) {
	byName, _, err := LoadCaptureDescriptors(opts)
	if err != nil {
		return nil, err
	}
	return &Registry{byName: byName}, nil
}

// DefaultRegistry builds a Registry from the embedded built-in defaults
// plus the standard user descriptor layer path
// ([DefaultUserCapturePath]). It is the production constructor.
func DefaultRegistry() (*Registry, error) {
	return NewRegistry(DefaultCaptureLoadOptions())
}

// Resolve returns the descriptor registered under name. ok is false for an
// unknown name; the caller surfaces a clear "no such tool" error rather
// than this package guessing.
func (r *Registry) Resolve(name string) (CaptureDescriptor, bool) {
	d, ok := r.byName[name]
	return d, ok
}

// Names returns the registered descriptor names in sorted order, for help
// or usage output.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.byName))
	for n := range r.byName {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Bind resolves name, builds a Driver via New (resolving the runtime and
// defaulting the Runner), applies the descriptor onto it with the
// caller-resolved image and store seam, and returns the ready Driver. It
// is the convenience path from a tool name to an executable Driver. An
// unknown name is an error naming the registered tools.
func (r *Registry) Bind(name, runtime, image string, store StoreFunc) (*Driver, error) {
	d, ok := r.Resolve(name)
	if !ok {
		return nil, fmt.Errorf("capture: no descriptor registered for %q (have %v)", name, r.Names())
	}
	drv, err := New(runtime, image)
	if err != nil {
		return nil, err
	}
	d.Apply(drv, image, store)
	return drv, nil
}
