// Package main is the source of the shared spawn-forwarder WASM
// embedded into the Aileron daemon binary (per ADR-0002's
// spawn-primitive section as folded in #592).
//
// The forwarder is a manifest-driven adapter between the action
// layer's JSON envelope ({op, args}) and the runtime's
// aileron_host.spawn_op host function. It carries no service-specific
// knowledge and no capability declarations of its own. Every
// behavior decision lives in the manifest of the connector that
// references it.
//
// Build (committed binary lives at internal/sandbox/forwarder/spawn-forwarder.wasm):
//
//	cd internal/sandbox/forwarder/src && \
//	  GOOS=wasip1 GOARCH=wasm go build -trimpath -ldflags="-s -w" \
//	    -o ../spawn-forwarder.wasm .
//
//go:build wasip1

package main

import (
	"encoding/json"
	"io"
	"os"
	"unsafe"
)

//go:wasmimport aileron_host spawn_op
//go:noescape
func hostSpawnOp(opPtr unsafe.Pointer, opLen uint32, argsPtr unsafe.Pointer, argsLen uint32) int32

//go:wasmimport aileron_host spawn_status
//go:noescape
func hostSpawnStatus() int32

//go:wasmimport aileron_host spawn_output_size
//go:noescape
func hostSpawnOutputSize(which uint32) int32

//go:wasmimport aileron_host spawn_output_read
//go:noescape
func hostSpawnOutputRead(which uint32, dstPtr unsafe.Pointer, dstLen uint32) int32

// ptr returns a stable pointer to the first byte of b. Empty slices
// get a sentinel address (uint pointer arithmetic on a nil pointer is
// undefined behavior under wasip1).
func ptr(b []byte) unsafe.Pointer {
	if len(b) == 0 {
		return unsafe.Pointer(&emptyPtrSentinel[0])
	}
	return unsafe.Pointer(&b[0])
}

var emptyPtrSentinel = [1]byte{}

type input struct {
	Op   string            `json:"op"`
	Args map[string]string `json:"args"`
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
		writeError("connector_runtime_error", err.Error())
		os.Exit(1)
	}
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		writeError("connector_runtime_error", "invalid input envelope: "+err.Error())
		os.Exit(1)
	}
	if in.Op == "" {
		writeError("connector_runtime_error", "op is required")
		os.Exit(1)
	}

	opBytes := []byte(in.Op)
	var argsBytes []byte
	if in.Args != nil {
		argsBytes, _ = json.Marshal(in.Args)
	}

	rc := hostSpawnOp(
		ptr(opBytes), uint32(len(opBytes)),
		ptr(argsBytes), uint32(len(argsBytes)),
	)
	if rc != 0 {
		// The host stuck a structured error on the per-invocation
		// state; the runtime surfaces it to the caller. We emit a
		// minimal envelope so the connector's exit is informative.
		writeError("capability_denied", "spawn_op host function denied (rc != 0); see audit log")
		os.Exit(0)
	}

	exit := hostSpawnStatus()
	stdout := readSpawnOutput(0)
	stderr := readSpawnOutput(1)

	writeOutput(map[string]any{
		"exit":   int(exit),
		"stdout": string(stdout),
		"stderr": string(stderr),
	})
}

// readSpawnOutput pulls the named output stream out of per-invocation
// state via the spawn_output_size + spawn_output_read pair.
func readSpawnOutput(which uint32) []byte {
	size := hostSpawnOutputSize(which)
	if size <= 0 {
		return nil
	}
	buf := make([]byte, size)
	n := hostSpawnOutputRead(which, ptr(buf), uint32(size))
	if n <= 0 {
		return nil
	}
	return buf[:n]
}

func writeOutput(out map[string]any) {
	_ = json.NewEncoder(os.Stdout).Encode(output{Output: out})
}

func writeError(class, message string) {
	_ = json.NewEncoder(os.Stdout).Encode(output{Error: &outputError{Class: class, Message: message}})
}
