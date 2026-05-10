# Shared spawn-forwarder

`spawn-forwarder.wasm` is the daemon-embedded WASM that drives every
spawn-primitive connector whose manifest declares
`connector.forwarder = "builtin://spawn-forwarder"`. See
[ADR-0002](../../../docs/src/content/docs/adr/0002-connector-model.md)
for the design.

## Regenerate

```sh
cd internal/sandbox/forwarder/src && \
  GOOS=wasip1 GOARCH=wasm go build -trimpath -ldflags="-s -w" \
    -o ../spawn-forwarder.wasm .
```

Or via the Taskfile:

```sh
task build:forwarder
```

The resulting `~3 MB` binary is checked in. It exposes a single WASI
`_start` entrypoint that reads `{"op": "...", "args": {...}}` from
stdin, calls `aileron_host.spawn_op`, reads back exit code and
stdout/stderr via the `spawn_output_*` host functions, and emits a
JSON output envelope to stdout.

The forwarder carries no service-specific knowledge. All behavior is
driven by the consuming connector's manifest.
