// Package forwarder embeds the shared spawn-forwarder WASM into the
// Aileron daemon binary.
//
// The forwarder is a manifest-driven adapter between the action layer's
// JSON envelope ({op, args}) and the runtime's aileron_host.spawn_op
// host function. Per ADR-0002's spawn-primitive section, every
// spawn-primitive connector that references `connector.forwarder =
// "builtin://spawn-forwarder"` is dispatched through this binary.
//
// The forwarder source lives under src/. Regenerate the embedded WASM
// with the build command in src/main.go's doc comment or via the
// Taskfile's `build:forwarder` task.
package forwarder

import _ "embed"

// WASM is the compiled forwarder bytes. Loaded into the Wazero runtime
// when a connector manifest declares
// `connector.forwarder = "builtin://spawn-forwarder"`.
//
// The hash of these bytes contributes to a forwarder-connector's
// content hash (sha256(WASM || manifest.toml) per ADR-0002), so a
// daemon upgrade that ships new forwarder bytes invalidates the cached
// hashes of every shared-forwarder wrap on the host. The install
// pipeline re-stores the manifest under the new hash; manifest
// content is unchanged.
//
//go:embed spawn-forwarder.wasm
var WASM []byte
