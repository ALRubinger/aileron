# Sandbox test fixtures

`echo.wasm` is a precompiled WASM binary used by `internal/sandbox/wazero_test.go`.
It is built from `echo/main.go` against Go's native WASI Preview 1 target.

## Regenerate

```sh
cd internal/sandbox/testdata/echo && \
  GOOS=wasip1 GOARCH=wasm go build -trimpath -ldflags="-s -w" -o ../echo.wasm .
```

The resulting binary is checked in (~3 MB). It exposes a single
WASI `_start` entrypoint that reads `{"op":"...","args":{...}}` from
stdin and writes a JSON envelope to stdout. See `echo/main.go` for the
supported ops (`echo`, `log`, `http`, `loop`, `panic`).

The fixture exercises the v1 Aileron host-import ABI
(`aileron_host.log`, `aileron_host.http_request`,
`aileron_host.http_response_*`). When Wazero ships WASI Preview 2 /
Component Model support, this fixture stays valid — the v1 ABI remains
a parallel host-import surface.
