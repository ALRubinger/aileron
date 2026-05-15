---
title: "Authoring a Connector"
description: "Write a connector from scratch: project layout, the JSON I/O envelope, host functions, credential mediation, the manifest, and the build loop."
---

This guide walks you from an empty directory to a working connector binary. By the end you will have a `wasip1` Go program that reads a JSON envelope from stdin, calls an external HTTP API through the runtime, and writes a JSON result to stdout.

If you have not read it yet, start with [Connectors](/concepts/connectors/) for the model. This guide is the *how*; that page is the *what* and *why*. Once your connector compiles and behaves the way you want, [Publishing a Connector](/guides/publishing-a-connector/) covers signing, tagging, and the keyring trust model.

The reference implementation throughout this guide is [`github.com/ALRubinger/aileron-connector-google`](https://github.com/ALRubinger/aileron-connector-google) — a real connector exposing six Gmail and Calendar ops. Snippets here are simplified for clarity; clone that repo when you want production-shape code to study.

## What you are building

A connector is a single `wasip1` Go binary plus a `manifest.toml`. The binary is invoked once per call: the runtime spins up a fresh sandbox instance, pipes a JSON envelope into stdin, runs `_start`, reads the JSON envelope from stdout, and tears the instance down. There is no long-running process and no shared state between invocations.

Two things bound what your code can do:

- The **manifest** declares network hosts, credential kind, and host functions. The sandbox refuses anything not declared.
- The **host ABI** is a fixed set of functions you import as `aileron_host.*`. Everything outside the sandbox — network, credentials, log output — happens through these.

You write Go that targets `GOOS=wasip1 GOARCH=wasm`. No goroutines that need a real scheduler, no filesystem access beyond stdio, no syscalls outside the WASI Preview 1 set plus `aileron_host`.

## Project skeleton

The minimum, mirroring the layout of the reference repo:

```
my-connector/
├── connector/
│   ├── main.go              # //go:build wasip1 — the WASM entry point
│   ├── helpers.go           # untagged — pure helpers, host-testable
│   ├── helpers_test.go      # standard go test, runs on the host
│   ├── go.mod
│   └── manifest.toml
└── Taskfile.yml             # build wasm, smoke-compile wasip1, run unit tests
```

Two source files instead of one is a deliberate split. `main.go` carries the `//go:build wasip1` tag because it imports the `aileron_host` host module — code Go can only compile when targeting the WASM runtime that supplies those imports. `helpers.go` has no build tag, so any pure logic that lives there (URL building, body encoding, attendee normalization) can be unit-tested with `go test` on your host platform without going through the WASM build at all.

The publishing guide describes the full `aileron-connector-<service>` repo layout (action templates, signing keys, release tarball). For authoring, only `connector/` matters.

## The I/O envelope

Every invocation reads one JSON object from stdin and writes one JSON object to stdout. The shapes are fixed:

**Input:**

```json
{
  "op": "list_recent_emails",
  "args": { "query": "is:unread", "max_results": 10 }
}
```

**Output (success):**

```json
{ "output": { "messages": [...], "resultSizeEstimate": 42 } }
```

**Output (error):**

```json
{ "error": { "class": "external_api_error", "message": "Gmail API returned 429: rate limited" } }
```

`op` is a string your connector dispatches on. `args` is an arbitrary JSON object — whatever the action template passed in. `output` is whatever you want to return; the runtime hands it back to the caller verbatim. `error.class` is one of the canonical failure classes the runtime understands; `error.message` is a human-readable string.

Action manifests can declare scalar input types (`string`, `integer`, `number`, `boolean`) or structured types (`array`, `object`). Scalar args arrive as the matching Go scalar inside `args` (`string`, `float64`, `bool`); structured args arrive as decoded `[]any` / `map[string]any` payloads exactly as the LLM produced them. There is no per-item or per-property typing in the manifest layer, so your connector is the source of truth for semantic shape — validate required fields and types at op dispatch, and return `connector_runtime_error` with a precise message when the LLM's payload is malformed. See [Authoring an action § Structured inputs](/guides/authoring-an-action/#structured-inputs) for what an author types into the manifest.

Two reference points worth bookmarking:

- [`connector/main.go`](https://github.com/ALRubinger/aileron-connector-google/blob/main/connector/main.go) in `aileron-connector-google` is a complete production connector — six ops, four host functions, real Google API calls.
- `internal/sandbox/testdata/echo/main.go` in the Aileron repo is the test fixture; it exercises every host function in isolation against a real test harness.

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
		writeError("connector_runtime_error", "read_stdin: "+err.Error())
		os.Exit(1)
	}
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		writeError("connector_runtime_error", "parse_input: "+err.Error())
		os.Exit(1)
	}

	switch in.Op {
	case "ping":
		writeOutput(map[string]any{"ok": true})
	default:
		writeError("connector_runtime_error", "unknown op: "+in.Op)
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

Then dispatch a real op (this is the simplified shape of `list_recent_emails` from the reference repo):

```go
case "list_recent_emails":
	q := url.Values{}
	q.Set("maxResults", "10")
	target := "https://gmail.googleapis.com/gmail/v1/users/me/messages?" + q.Encode()

	status, body, err := doRequest(httpRequest{
		Method:     "GET",
		URL:        target,
		Headers:    map[string]string{"Accept": "application/json"},
		Credential: "oauth2",
	})
	if err != nil {
		writeError("connector_runtime_error", "list_recent_emails: "+err.Error())
		return
	}
	if status < 200 || status >= 300 {
		writeError("external_api_error",
			fmt.Sprintf("Gmail API returned %d: %s", status, string(body)))
		return
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		writeError("connector_runtime_error", "list_recent_emails: parse: "+err.Error())
		return
	}
	writeOutput(parsed)
```

Two-status-class checks (`< 200 || >= 300`) instead of `>= 400` is the right shape: 3xx redirects still mean "didn't get the data".

## Credentials: the connector never sees the bytes

When the request envelope carries a `credential` field, the runtime resolves the user's bound credential, attaches `Authorization: Bearer <token>` host-side, and forwards the request. The token never crosses the sandbox boundary. There is no `vault.read("...")` API and there will not be one — credential access is mediated, not granted.

The string you pass in `credential` must match the kind your manifest declared in `[capabilities.credential].kind` (`api_key` or `oauth2` for v1). Asking for any other kind is `capability_denied` at the sandbox boundary.

If the user has not bound a credential to your connector, the runtime returns `binding_required` and the call fails before any network dial. You do not need to check anything yourself — request the credential, handle the denial like any other failure.

## Error classes

The runtime defines a closed set of canonical failure classes in [`internal/failure/failure.go`](https://github.com/ALRubinger/aileron/blob/main/internal/failure/failure.go). Two are emitted by the connector itself:

| Class | When |
|---|---|
| `external_api_error` | The remote API returned a non-2xx status, a malformed response, or otherwise indicated the call failed upstream. |
| `connector_runtime_error` | Something went wrong inside the connector itself — bad input from the action (missing required arg, wrong type), JSON parse failure, an HTTP call that returned `rc != 0`, an unknown op. |

Two are emitted by the runtime *to* the connector (you observe them but don't write them yourself):

- `capability_denied` — the call exceeded the connector's manifest grant or the action's declared capability subset.
- `binding_required` — the connector requested a credential and no binding exists.

Use `external_api_error` when the failure is on the other side of the network; use `connector_runtime_error` for everything else you originate. Stable class names let action templates and the runtime treat failures uniformly. If you write a class outside this set, it propagates verbatim, but the runtime's mapping logic will treat it as opaque.

## The manifest

Declare exactly what the connector touches. Every field is enforced. This is the manifest from `aileron-connector-google`, lightly trimmed:

```toml
[connector]
name = "github://ALRubinger/aileron-connector-google"
version = "0.0.0-dev"   # CI substitutes the real version at release time
publisher = "ALRubinger"

[capabilities.network]
hosts = [
  "gmail.googleapis.com:443",
  "www.googleapis.com:443",
]

[capabilities.credential]
kind = "oauth2"
scope = "Read your Gmail messages and Calendar events; create drafts and calendar events"

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
- `scope` here is **human-readable prose** — what the user sees in install consent prompts. It is *not* the OAuth scope list; that lives in the `[capabilities.credential.oauth2]` block (see below).
- `imports` lists the host functions you actually import. Declaring an import you never call is harmless; calling one you did not declare is a load-time failure.
- `version = "0.0.0-dev"` is intentional in source. The release workflow substitutes the real version (extracted from the pushed tag) into a build copy before signing — the publisher never hand-edits version fields. Same pattern applies to action manifests.

OAuth2 connectors need a `[capabilities.credential.oauth2]` block declaring the provider endpoints, client_id, and scope list. That block is covered in [Publishing a Connector](/guides/publishing-a-connector/) since it's tied to OAuth-app registration with the upstream provider — and, for some providers (notably Google), to a release-time `client_secret` substitution.

## Limits

Per-call defaults from [ADR-0005](/adr/0005-sandbox-choice/):

- **Memory:** 64 MiB per instance, hard ceiling 1 GiB.
- **Wall time:** 30 s per invocation, hard ceiling 5 min.

If your connector needs more, request it in the manifest — the runtime clamps requests above the ceiling. Most connectors stay well under the defaults; if you are getting close, you are probably buffering a response you should be streaming or batching a job that should be paginated.

The wall-time clock starts when `_start` runs and stops when it returns. There is no separate per-request budget; one slow upstream burns the whole budget.

## Idempotency

Mark every op as either idempotent or not in your head as you write it. Read ops (GETs) are idempotent by their HTTP shape. Write ops that mutate the world (`draft_email`, `send_email`, `create_calendar_event`) are typically not — repeating them creates duplicates. The action manifest declares this per step (`[[execute]].idempotent = false`), and the runtime's retry layer ([ADR-0010](/adr/0010-failure-handling/)) honors the declaration: idempotent ops can retry on transient failure; non-idempotent ops never auto-retry.

This is not enforced at the connector boundary — it is a contract between the connector author (who knows whether the op is repeatable) and the action author (who declares the flag). Document idempotency for each op in your README and in the `main.go` doc comments so the action author knows what to declare.

## The build and iterate loop

Build:

```sh
task build
# or, equivalently:
GOOS=wasip1 GOARCH=wasm go build -trimpath -ldflags='-s -w' -o connector.wasm ./connector
```

`-trimpath` and `-ldflags='-s -w'` keep binaries reproducible and small (the `.wasm` ends up content-addressed in the user's store, so smaller is faster to fetch).

The reference repo's `Taskfile.yml` also has two test targets worth copying:

```sh
task test:unit     # runs go test against helpers.go on the host platform
task test:wasip1   # GOOS=wasip1 GOARCH=wasm go build -o /dev/null . — catches host-import signature mismatches
```

Pure helpers (URL construction, body encoding, normalization) belong in `helpers.go` so `task test:unit` exercises them directly. Anything that calls `aileron_host.*` lives in `main.go` behind the `//go:build wasip1` tag.

For end-to-end testing against real upstream APIs, point [`aileron-connector-dev-run`](https://github.com/ALRubinger/aileron/tree/main/cmd/aileron-connector-dev-run) at your `connector.wasm` + `manifest.toml` with a stub OAuth token (e.g. one minted via Google's OAuth Playground for a Google connector). It loads the binary into the production Wazero runtime, enforces the manifest's network grant, and invokes ops directly — validating credential mediation and the API call shape end-to-end without going through release / install / binding setup.

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
