package proxybinding

import "embed"

// builtinDefaults holds the descriptors Aileron ships as the built-in
// layer of the two-layer config convention (built-in -> user). Each file
// under defaults/ is a community profile that seals a
// third-party CLI by descriptor alone. linear.yaml is the proving
// example; adding another vendor is a new file here, never new proxy code.
//
//go:embed defaults/*.yaml
var builtinDefaults embed.FS

// builtinDefaultsDir is the directory the embed glob roots at.
const builtinDefaultsDir = "defaults"
