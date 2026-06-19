package capture

import "embed"

// builtinCaptureDefaults holds the capture descriptors Aileron ships as
// the built-in (trusted) layer of the two-layer config convention
// (built-in -> user). Each file under defaults/ declares one tool's
// credential-acquisition knowledge as data. gh.yaml is the one shipped
// example; adding another tool is a new file here, never new Go.
//
//go:embed defaults/*.yaml
var builtinCaptureDefaults embed.FS

// builtinCaptureDefaultsDir is the directory the embed glob roots at.
const builtinCaptureDefaultsDir = "defaults"
