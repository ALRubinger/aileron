package capture

import (
	"os"
	"path/filepath"
)

// DefaultUserCapturePath returns the per-user capture-descriptor file
// path, `~/.aileron/capture-descriptors.yaml`, the highest-precedence
// layer of the two-layer config convention. When the home directory
// cannot be resolved it falls back to a relative path under `.aileron`,
// matching the rest of the config handling. The file need not exist; an
// absent user layer contributes no descriptors.
func DefaultUserCapturePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".aileron", "capture-descriptors.yaml")
	}
	return filepath.Join(home, ".aileron", "capture-descriptors.yaml")
}

// DefaultCaptureLoadOptions returns the standard user descriptor layer
// path. The built-in defaults layer is always embedded; the user path
// selects the optional override layer.
func DefaultCaptureLoadOptions() CaptureLoadOptions {
	return CaptureLoadOptions{
		UserPath: DefaultUserCapturePath(),
	}
}
