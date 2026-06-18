package proxybinding

import (
	"os"
	"path/filepath"
)

// DefaultUserPath returns the per-user descriptor file path,
// `~/.aileron/binding-descriptors.yaml`, the highest-precedence layer of
// the two-layer config convention. When the home directory cannot be
// resolved it falls back to a relative path under `.aileron`, matching the
// rest of the config package's home-dir handling. The file need not exist;
// an absent user layer contributes no entries.
func DefaultUserPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".aileron", "binding-descriptors.yaml")
	}
	return filepath.Join(home, ".aileron", "binding-descriptors.yaml")
}

// DefaultLoadOptions returns the standard user descriptor layer path for
// daemon construction. The built-in defaults layer is always embedded; the
// user path selects the optional override layer.
func DefaultLoadOptions() LoadOptions {
	return LoadOptions{
		UserPath: DefaultUserPath(),
	}
}
