package proxybinding

import (
	"os"
	"path/filepath"
)

// DefaultUserPath returns the per-user descriptor file path,
// `~/.aileron/binding-descriptors.yaml`, the highest-precedence layer of
// the three-layer config convention. When the home directory cannot be
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

// DefaultProjectPath returns the project-layer descriptor file path,
// `.aileron/binding-descriptors.yaml` relative to the given working
// directory (the middle layer). An empty workdir roots it at the current
// directory. The file need not exist.
//
// For the running daemon this resolves against the daemon's working
// directory, not the directory a `launch` invocation was issued from: the
// binding table is loaded once at daemon construction and is daemon-global.
// Per-launch resolution against the launch repo root is deferred future
// work.
func DefaultProjectPath(workdir string) string {
	return filepath.Join(workdir, ".aileron", "binding-descriptors.yaml")
}

// DefaultLoadOptions returns the standard project and user descriptor
// layer paths for daemon construction. The built-in defaults layer is
// always embedded; these two paths select the optional override layers.
func DefaultLoadOptions(workdir string) LoadOptions {
	return LoadOptions{
		ProjectPath: DefaultProjectPath(workdir),
		UserPath:    DefaultUserPath(),
	}
}
