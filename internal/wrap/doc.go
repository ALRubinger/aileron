// Package wrap implements the connector-authoring tool that turns a
// local CLI binary into a spawn-primitive Aileron connector.
//
// The tool produces a complete connector source tree from one of two
// inputs:
//
//   - **YAML config (MCPShell-style precise control)**: a hand-authored
//     YAML file describing the connector's identity, the binary it
//     wraps, the subcommands it exposes, and the parameters each
//     subcommand takes. Use this mode when --help parsing would not
//     produce the right shape.
//
//   - **--help parsing (cli2mcp-style heuristic)**: invoke the named
//     binary with `--help`, parse the subcommand list, and emit a
//     scaffold the connector author edits. Use this for a 30-second
//     scaffold rather than a hand-authored connector.
//
// The output tree mirrors the v1 publisher's guide: a connector
// manifest.toml under `connector/`, an action.md per exposed
// subcommand under `actions/`, a Taskfile.yml, and the publisher's
// CI workflow stub.
//
// The emitted manifest's [capabilities.spawn] block names the wrapped
// binary, the argv patterns the connector will call, the env keys
// it forwards, and the filesystem scopes it touches. Output passes
// cstore.ValidateManifest by construction.
//
// See ADR-0002 for the spawn primitive, ADR-0014 for the per-platform
// sandbox mechanism the runtime applies, and the publisher's guide at
// docs/src/content/docs/guides/publishing-a-connector.md for the
// canonical layout of a connector source tree.
package wrap
