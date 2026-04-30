// Package main is the test fixture WASM connector for internal/sandbox.
//
// Build with:
//
//	GOOS=wasip1 GOARCH=wasm go build -o ../echo.wasm .
//
// (also reachable via the package-level go:generate directive in
// internal/sandbox).
//
// The fixture reads a JSON envelope `{"op": "...", "args": {...}}` from
// stdin and writes a JSON envelope `{"output": {...}}` (or
// `{"error": {...}}`) to stdout. The supported ops are intentionally
// shaped to exercise each acceptance criterion of issue #359 in
// isolation:
//
//	"echo"          — round-trips args.value to output (happy path)
//	"log"           — calls aileron_host.log; produces empty output
//	"http"          — calls aileron_host.http_request with args.url
//	"loop"          — infinite loop (resource-limit test)
//	"panic"         — calls os.Exit(1) (connector_runtime_error test)
//
// The fixture is compiled offline; the resulting .wasm is the test
// binary checked in at internal/sandbox/testdata/echo.wasm. Tests
// import the .wasm via go:embed.
//
//go:build wasip1

package main

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"unsafe"
)

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

func ptr(b []byte) unsafe.Pointer {
	if len(b) == 0 {
		return unsafe.Pointer(&_emptyPtrSentinel[0])
	}
	return unsafe.Pointer(&b[0])
}

var _emptyPtrSentinel = [1]byte{}

func aileronLog(level, message string) {
	lb := []byte(level)
	mb := []byte(message)
	hostLog(ptr(lb), uint32(len(lb)), ptr(mb), uint32(len(mb)))
}

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
	case "echo":
		writeOutput(map[string]any{"echo": in.Args["value"]})

	case "log":
		aileronLog("info", "hello from connector")
		writeOutput(map[string]any{"logged": true})

	case "http":
		url, _ := in.Args["url"].(string)
		method, _ := in.Args["method"].(string)
		if method == "" {
			method = "GET"
		}
		req, _ := json.Marshal(map[string]any{"method": method, "url": url})
		rc := hostHTTPRequest(ptr(req), uint32(len(req)))
		if rc != 0 {
			writeOutput(map[string]any{"http_request_rc": int(rc)})
			return
		}
		size := hostHTTPResponseSize()
		status := hostHTTPResponseStatus()
		out := map[string]any{
			"status": int(status),
			"size":   int(size),
		}
		if size > 0 {
			body := make([]byte, size)
			n := hostHTTPResponseRead(ptr(body), uint32(size))
			if n > 0 {
				out["body"] = string(body[:n])
			}
		}
		writeOutput(out)

	case "loop":
		// Infinite loop — exercises wall-time / fuel termination.
		for {
			_ = binary.LittleEndian
		}

	case "panic":
		writeError("connector_runtime_error", "intentional panic")
		os.Exit(1)

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
