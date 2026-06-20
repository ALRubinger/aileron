package capture

import "embed"

// builtinCaptureDefaults holds the capture descriptors Aileron ships as
// the built-in (trusted) layer of the two-layer config convention
// (built-in -> user). Each `.yaml` file under defaults/ declares one
// tool's credential-acquisition knowledge as data.
//
// The built-in layer is now empty by design: gh moved to its devcontainer
// Feature CLI unit (#1323), so the directory carries only a README.md
// placeholder and no `.yaml` files. The embed roots at the whole directory
// (not a `*.yaml` glob, which would be a compile error against a directory
// with no matching files) and parseBuiltinCaptureDefaults filters to
// `.yaml`, so the README is skipped. Adding a new built-in tool is a new
// `.yaml` file here, never new Go.
//
//go:embed all:defaults
var builtinCaptureDefaults embed.FS

// builtinCaptureDefaultsDir is the directory the embed glob roots at.
const builtinCaptureDefaultsDir = "defaults"
