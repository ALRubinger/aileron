---
title: "Authoring a Connector"
description: "Write a connector from scratch: project layout, the JSON I/O envelope, host functions, credential mediation, the manifest, and the build loop."
---

This guide walks you from an empty directory to a working connector binary. By the end you will have a `wasip1` Go program that reads a JSON envelope from stdin, calls an external HTTP API through the runtime, and writes a JSON result to stdout.

If you have not read it yet, start with [Connectors](/concepts/connectors/) for the model. This guide is the *how*; that page is the *what* and *why*. Once your connector compiles and behaves the way you want, [Publishing a connector](/guides/publishing-a-connector/) covers signing, tagging, and the keyring trust model.

## What you are building

A connector is a single `wasip1` Go binary plus a `manifest.toml`. The binary is invoked once per call: the runtime spins up a fresh sandbox instance, pipes a JSON envelope into stdin, runs `_start`, reads the JSON envelope from stdout, and tears the instance down. There is no long-running process and no shared state between invocations.

Two things bound what your code can do:

- The **manifest** declares network hosts, credential kind, and host functions. The sandbox refuses anything not declared.
- The **host ABI** is a fixed set of functions you import as `aileron_host.*`. Everything outside the sandbox — network, credentials, log output — happens through these.

You write Go that targets `GOOS=wasip1 GOARCH=wasm`. No goroutines that need a real scheduler, no filesystem access beyond stdio, no syscalls outside the WASI Preview 1 set plus `aileron_host`.

## Project skeleton

The minimum:

```
my-connector/
├── connector/
│   ├── main.go
│   └── manifest.toml
└── Taskfile.yml          # optional but recommended
```

The publishing guide describes the full `aileron-connector-<service>` repo layout (action templates, signing keys, release tarball). For authoring, only `connector/` matters.

## The I/O envelope

Every invocation reads one JSON object from stdin and writes one JSON object to stdout. The shapes are fixed:

**Input:**

```json
{
  "op": "list_channels",
  "args": { "team_id": "T123" }
}
```

**Output (success):**

```json
{ "output": { "channels": [...] } }
```

**Output (error):**

```json
{ "error": { "class": "external_service_error", "message": "rate limited" } }
```

`op` is a string your connector dispatches on. `args` is an arbitrary JSON object — whatever the action template passed in. `output` is whatever you want to return; the runtime hands it back to the caller verbatim. `error.class` is a stable identifier the runtime maps onto its structured error model; `error.message` is a human-readable string.

The canonical reference is `internal/sandbox/testdata/echo/main.go` in the Aileron repo — every host function is exercised there, in isolation, against a real test harness.

## A minimal main

Start with this skeleton. It reads stdin, parses the envelope, dispatches on `op`, and writes a result.

```go
//go:build wasip1

package main

import (
	"encoding/json"
	"io"
	"os"
)

type input struct {
	Op   string         `json:"op"`
	Args map[string]any `json:"args"`
}

type output struct {
	Output map[string]any `json:"output,omitempty"`
	Error  *outputError   `json:"error,omitempty"`
}

type outputError struct {
	Class   string `json:"class"`
	Message string `json:"message"`
}

func main() {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		writeError("read_stdin", err.Error())
		os.Exit(1)
	}
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		writeError("parse_input", err.Error())
		os.Exit(1)
	}

	switch in.Op {
	case "ping":
		writeOutput(map[string]any{"ok": true})
	default:
		writeError("unknown_op", in.Op)
	}
}

func writeOutput(out map[string]any) {
	_ = json.NewEncoder(os.Stdout).Encode(output{Output: out})
}

func writeError(class, message string) {
	_ = json.NewEncoder(os.Stdout).Encode(output{Error: &outputError{Class: class, Message: message}})
}
```

This is enough to compile, install, and invoke. Build:

```sh
cd connector
GOOS=wasip1 GOARCH=wasm go build -o ../build/connector.wasm .
```

## The host ABI

Five host functions, all imported under the `aileron_host` module. Every escape hatch out of the sandbox runs through one of them.

```go
//go:wasmimport aileron_host log
//go:noescape
func hostLog(levelPtr unsafe.Pointer, levelLen uint32, msgPtr unsafe.Pointer, msgLen uint32)

//go:wasmimport aileron_host http_request
//go:noescape
func hostHTTPRequest(reqPtr unsafe.Pointer, reqLen uint32) int32

//go:wasmimport aileron_host http_response_size
//go:noescape
func hostHTTPResponseSize() int32

//go:wasmimport aileron_host http_response_status
//go:noescape
func hostHTTPResponseStatus() int32

//go:wasmimport aileron_host http_response_read
//go:noescape
func hostHTTPResponseRead(dstPtr unsafe.Pointer, dstLen uint32) int32
```

`log` is fire-and-forget structured logging (level + message). The runtime captures the lines and emits them through its own logger so they show up alongside everything else in `aileron launch` output.

The four `http_*` functions are a single request/response pair, split across calls because the WASM ABI passes everything by raw pointer. The pattern is always the same: marshal a request envelope, call `http_request`, then read back size, status, and body.

You will need a small `ptr` helper because `unsafe.Pointer` to an empty slice is undefined:

```go
var _emptyPtrSentinel = [1]byte{}

func ptr(b []byte) unsafe.Pointer {
	if len(b) == 0 {
		return unsafe.Pointer(&_emptyPtrSentinel[0])
	}
	return unsafe.Pointer(&b[0])
}
```

## Calling an external API

`http_request` takes a JSON envelope. The runtime parses it host-side, validates the URL against your manifest's network grant, and either dials or denies.

```go
type httpRequest struct {
	Method     string            `json:"method"`
	URL        string            `json:"url"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       string            `json:"body,omitempty"`
	Credential string            `json:"credential,omitempty"`
}

func doRequest(req httpRequest) (status int, body []byte, err error) {
	raw, _ := json.Marshal(req)
	rc := hostHTTPRequest(ptr(raw), uint32(len(raw)))
	if rc != 0 {
		return 0, nil, fmt.Errorf("http_request failed: rc=%d", rc)
	}
	status = int(hostHTTPResponseStatus())
	size := hostHTTPResponseSize()
	if size > 0 {
		body = make([]byte, size)
		n := hostHTTPResponseRead(ptr(body), uint32(size))
		body = body[:n]
	}
	return status, body, nil
}
```

`rc` of `0` means success; `-1` means the request was denied (capability gate, build error, network failure) or otherwise failed; `-2` means your envelope was malformed JSON. On any non-zero `rc` the runtime has already attached a structured error to the call result — your connector cannot recover and should write a matching `outputError` and exit.

Then dispatch a real op:

```go
case "list_channels":
	status, body, err := doRequest(httpRequest{
		Method:     "GET",
		URL:        "https://slack.com/api/conversations.list",
		Credential: "oauth2",
	})
	if err != nil {
		writeError("connector_runtime_error", err.Error())
		return
	}
	if status >= 400 {
		writeError("external_service_error",
			fmt.Sprintf("slack returned %d: %s", status, string(body)))
		return
	}
	var parsed map[string]any
	json.Unmarshal(body, &parsed)
	writeOutput(map[string]any{"channels": parsed["channels"]})
```

## Credentials: the connector never sees the bytes

When the request envelope carries a `credential` field, the runtime resolves the user's bound credential, attaches `Authorization: Bearer <token>` host-side, and forwards the request. The token never crosses the sandbox boundary. There is no `vault.read("...")` API and there will not be one — credential access is mediated, not granted.

The string you pass in `credential` must match the kind your manifest declared in `[capabilities.credential].kind` (`api_key` or `oauth2` for v1). Asking for any other kind is `capability_denied` at the sandbox boundary.

If the user has not bound a credential to your connector, the runtime returns `binding_required` and the call fails before any network dial. You do not need to check anything yourself — request the credential, handle the denial like any other failure.

## Error classes

The runtime recognizes these error classes from your output. Use them precisely; users see them in audit logs and `aileron launch` traces:

| Class | When |
|---|---|
| `bad_input` | The action passed `args` that violated your op's contract (missing fields, bad types). |
| `external_service_error` | The remote API returned an error (4xx, 5xx, malformed response). |
| `connector_runtime_error` | Something went wrong inside the connector itself — JSON parse failure, unexpected nil, an HTTP call that returned `rc != 0`. |
| `unknown_op` | The action requested an op your connector does not implement. |

If you write a class outside this set, it propagates verbatim — but stick to these unless you have a specific reason. Stable class names let action templates and the runtime treat failures uniformly.

## The manifest

Declare exactly what the connector touches. Every field is enforced.

```toml
[connector]
name = "github://acme/aileron-connector-slack"
version = "0.1.0"
publisher = "Acme"

[capabilities.network]
hosts = ["slack.com:443"]

[capabilities.credential]
kind = "oauth2"
scope = "Read channels and post messages"

[capabilities.runtime]
imports = [
  "log",
  "http_request",
  "http_response_size",
  "http_response_status",
  "http_response_read",
]
```

A few non-obvious rules:

- `hosts` is a closed list of `host:port` pairs. No wildcards. Forgetting `:443` is a common mistake — every entry needs an explicit port. The runtime denies any URL whose `host:port` is not on the list.
- `kind` must match the kind on the user's binding. If your code passes `credential: "oauth2"` but the manifest declares `api_key`, the request is denied at the sandbox boundary even before the network gate fires.
- `imports` lists the host functions you actually import. Declaring an import you never call is harmless; calling one you did not declare is a load-time failure.

OAuth2 connectors need a `[capabilities.credential.oauth2]` block too — that part is covered in [Publishing a connector](/guides/publishing-a-connector/) since it's tied to OAuth-app registration with the upstream provider.

## Limits

Per-call defaults from [ADR-0005](/adr/0005-sandbox-choice/):

- **Memory:** 64 MiB per instance, hard ceiling 1 GiB.
- **Wall time:** 30 s per invocation, hard ceiling 5 min.

If your connector needs more, request it in the manifest — the runtime clamps requests above the ceiling. Most connectors stay well under the defaults; if you are getting close, you are probably buffering a response you should be streaming or batching a job that should be paginated.

The wall-time clock starts when `_start` runs and stops when it returns. There is no separate per-request budget; one slow upstream burns the whole budget.

## The build and iterate loop

Build:

```sh
GOOS=wasip1 GOARCH=wasm go build -trimpath -ldflags='-s -w' -o build/connector.wasm ./connector
```

`-trimpath` and `-ldflags='-s -w'` are not required for correctness, but they keep binaries reproducible and small (the resulting `.wasm` ends up content-addressed in the user's store, so smaller is faster to fetch).

To smoke-test before publishing, install the unsigned binary into a local store and invoke it through `aileron launch`. The exact `aileron connector install --local <path>` flow lives with the CLI; iterate there.

## Things that will trip you up

- **Goroutines.** `wasip1` has a single OS thread. Code that assumes true parallelism — long-running goroutines, blocking channel ops across goroutines — will deadlock or starve. Keep the connector single-threaded.
- **Time.** `time.Now()` works; `time.Sleep()` blocks but is bounded by the wall-time limit. There is no monotonic clock the runtime exposes separately.
- **Random.** `crypto/rand` works. Don't seed `math/rand` from `time.Now()` — same call shape, but `crypto/rand` is the right primitive when it matters.
- **Body size.** `http_response_size` returns the buffered body's full length. The runtime caps response bodies to keep one slow connector from holding pages of memory; very large responses are truncated.
- **Empty pointers.** Passing `unsafe.Pointer(&b[0])` for an empty `b` is undefined. Use the `ptr` helper above.

## Where to go next

Once your connector behaves the way you want:

- [Publishing a connector](/guides/publishing-a-connector/) — repo layout, signing, tag conventions, keyring trust, action templates.
- [ADR-0002: Connector Model](/adr/0002-connector-model/) — the design constraints behind everything in this guide.
- [ADR-0005: Sandbox Choice](/adr/0005-sandbox-choice/) — why WASM, why these limits, why this ABI.
