package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/tetratelabs/wazero/api"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/credential"
	"github.com/ALRubinger/aileron/internal/cstore"
)

// SpawnPolicy enforces a connector manifest's `[capabilities.spawn]`
// declaration (per ADR-0002 spawn primitive, ADR-0014 sandbox tech).
// Built once per connector and consulted on every spawn host-function
// call before any process is created.
//
// SpawnPolicy is the gate; the platform sandbox in `spawnExecutor` is
// the second-line enforcement. Both must agree for a call to proceed.
//
// The policy also carries the per-stream output caps the runtime
// applies to subprocess stdout and stderr (per [cstore.ManifestSpawnLimits]
// and ADR-0014's output-cap consequences). The caps are derived once
// from the manifest at policy construction and consulted on every
// spawn invocation.
type SpawnPolicy struct {
	// programs maps an absolute program path to its declared optional
	// content hash. An empty hash means "any bytes at this path are
	// allowed"; a non-empty hash pins exact bytes.
	programs map[string]string
	// primaryProgram is the first declared program path. The high-level
	// host function aileron_host.spawn_op uses this when constructing
	// envelopes from the manifest's operations table; per-program op
	// routing for multi-program connectors is a future extension that
	// would live on each operation entry.
	primaryProgram string
	// argvPatterns is the parsed allow-list of argv shapes. An incoming
	// argv must match at least one pattern after placeholder substitution.
	argvPatterns []argvPattern
	// operations maps op name to the parsed argv pattern declared in
	// [capabilities.spawn.operations]. aileron_host.spawn_op resolves
	// an op name against this map to substitute placeholders.
	operations map[string]argvPattern
	// envAllowed is the closed set of environment keys the runtime is
	// permitted to set on the subprocess. The connector cannot pass any
	// other key.
	envAllowed map[string]struct{}
	// envOrder is the manifest's declared env_passthrough sequence.
	// Used by spawn_op to populate the envelope env in a stable order
	// from os.Environ values.
	envOrder []string
	// credentialEnvKeys is the manifest's declared list of env keys
	// the runtime should fill with the resolved credential value.
	// Each entry is also in envOrder; spawn_op propagates the list
	// into the SpawnEnvelope.CredentialEnvKeys so processSpawn's
	// existing credential-injection path handles the rest.
	credentialEnvKeys []string
	// fsRead and fsWrite are the declared filesystem scopes the
	// platform sandbox is expected to enforce. The gate rejects
	// envelopes whose cwd falls outside fsRead.
	fsRead  []string
	fsWrite []string
	// cwd is the optional manifest-declared cwd policy. When set, an
	// envelope's cwd must match (or be empty, in which case the
	// runtime substitutes this value).
	cwd string
	// stdoutCap and stderrCap are the resolved byte caps the runtime
	// applies when capturing subprocess output. Resolved from the
	// manifest's [capabilities.spawn.limits] block at construction;
	// fall back to [cstore.DefaultMaxStdoutBytes] and
	// [cstore.DefaultMaxStderrBytes] when unset.
	stdoutCap int64
	stderrCap int64
}

// SpawnLimits carries the runtime-decided byte caps applied to a
// single spawn invocation's captured output. The runtime resolves
// these from the manifest (per [cstore.ManifestSpawnLimits]) and
// passes them to the [SpawnExecutor]; connectors do not control
// their own caps.
type SpawnLimits struct {
	// MaxStdoutBytes caps the bytes the executor returns in
	// [SpawnResult.Stdout]. Output past this point is dropped and
	// a structured truncation marker is appended to the captured
	// bytes.
	MaxStdoutBytes int64

	// MaxStderrBytes caps the bytes the executor returns in
	// [SpawnResult.Stderr]. Same truncation semantics as
	// MaxStdoutBytes.
	MaxStderrBytes int64
}

// StdoutCap returns the resolved stdout byte cap. Always positive
// when the policy was constructed from a valid manifest.
func (p *SpawnPolicy) StdoutCap() int64 { return p.stdoutCap }

// StderrCap returns the resolved stderr byte cap.
func (p *SpawnPolicy) StderrCap() int64 { return p.stderrCap }

// NewSpawnPolicy builds a SpawnPolicy from a connector manifest. A nil
// or absent `[capabilities.spawn]` block produces a deny-all policy:
// the connector did not declare spawn and may not invoke a subprocess.
func NewSpawnPolicy(m *cstore.Manifest) *SpawnPolicy {
	p := &SpawnPolicy{
		programs:   map[string]string{},
		envAllowed: map[string]struct{}{},
		operations: map[string]argvPattern{},
		stdoutCap:  cstore.DefaultMaxStdoutBytes,
		stderrCap:  cstore.DefaultMaxStderrBytes,
	}
	if m == nil || m.Capabilities.Spawn == nil {
		return p
	}
	s := m.Capabilities.Spawn
	for _, prog := range s.Programs {
		p.programs[prog.Path] = prog.Hash
	}
	if len(s.Programs) > 0 {
		p.primaryProgram = s.Programs[0].Path
	}
	for name, op := range s.Operations {
		parsed := parseArgvPattern(op.Argv)
		p.operations[name] = parsed
		p.argvPatterns = append(p.argvPatterns, parsed)
	}
	for _, k := range s.EnvPassthrough {
		p.envAllowed[k] = struct{}{}
	}
	p.envOrder = append(p.envOrder, s.EnvPassthrough...)
	p.credentialEnvKeys = append(p.credentialEnvKeys, s.CredentialEnvKeys...)
	p.fsRead = append(p.fsRead, s.FSRead...)
	p.fsWrite = append(p.fsWrite, s.FSWrite...)
	p.cwd = s.Cwd
	p.stdoutCap = s.StdoutCap()
	p.stderrCap = s.StderrCap()
	return p
}

// BuildEnvelopeFromOp resolves `opName` against the manifest's
// [capabilities.spawn.operations] table, substitutes `{name}`
// placeholders in the operation's argv pattern from `args`, and
// returns a fully-formed SpawnEnvelope ready for CheckSpawn /
// processSpawn.
//
// `getEnv` resolves the values for the manifest's env_passthrough keys
// from a source the caller chooses (e.g. os.Getenv in production; a
// fake in tests). Empty values are omitted from the envelope.
//
// Returns a structured *Error of class capability_denied when:
//   - The op name is not declared in the manifest's operations table
//     (boundary_detail: connector_manifest).
//   - A placeholder in the operation's argv has no matching arg
//     (boundary_detail: envelope, set by argvPattern.Substitute).
//   - The policy has no primary program (no programs declared).
func (p *SpawnPolicy) BuildEnvelopeFromOp(opName string, args map[string]string, getEnv func(string) string) (SpawnEnvelope, *Error) {
	if p == nil || len(p.operations) == 0 {
		return SpawnEnvelope{}, newCapabilityDenied(
			"spawn_op: connector did not declare any spawn operations",
			map[string]any{
				"requested":       "spawn_op:" + opName,
				"boundary_detail": "connector_manifest",
			})
	}
	pat, ok := p.operations[opName]
	if !ok {
		granted := make([]string, 0, len(p.operations))
		for k := range p.operations {
			granted = append(granted, k)
		}
		return SpawnEnvelope{}, newCapabilityDenied(
			"spawn_op: operation not declared in manifest",
			map[string]any{
				"requested":       "spawn_op:" + opName,
				"granted":         granted,
				"boundary_detail": "connector_manifest",
			})
	}
	if p.primaryProgram == "" {
		return SpawnEnvelope{}, newCapabilityDenied(
			"spawn_op: connector did not declare a program",
			map[string]any{
				"requested":       "spawn_op:" + opName,
				"boundary_detail": "connector_manifest",
			})
	}
	argv, denyErr := pat.Substitute(args)
	if denyErr != nil {
		return SpawnEnvelope{}, denyErr
	}
	env := map[string]string{}
	if getEnv != nil {
		for _, k := range p.envOrder {
			if v := getEnv(k); v != "" {
				env[k] = v
			}
		}
	}
	return SpawnEnvelope{
		Program:           p.primaryProgram,
		Argv:              argv,
		Env:               env,
		Cwd:               p.cwd,
		CredentialEnvKeys: append([]string(nil), p.credentialEnvKeys...),
	}, nil
}

// SpawnEnvelope is the JSON shape connectors marshal as input to
// `aileron_host.spawn`. Designed to be ergonomic across language ABIs;
// the runtime parses it host-side.
//
// Fields:
//
//   - Program: absolute path or ~/-anchored path of the binary to invoke.
//   - Argv: the full argument vector starting with argv[0]. The runtime
//     compares the argv (after placeholder elision) against the
//     manifest's argv_patterns.
//   - Env: the environment to set on the subprocess. Each key must be
//     in the manifest's env_passthrough; values are caller-supplied
//     except for keys in CredentialEnvKeys, which the runtime
//     resolves and injects.
//   - Cwd: optional working directory. Must match the manifest's cwd
//     (when set) and lie within fs_read.
//   - Stdin: optional bytes piped to the subprocess on stdin.
//   - CredentialEnvKeys: optional list of env keys whose values the
//     runtime should resolve from the connector's bound credential and
//     inject. The connector never holds the credential bytes.
type SpawnEnvelope struct {
	Program           string            `json:"program"`
	Argv              []string          `json:"argv"`
	Env               map[string]string `json:"env,omitempty"`
	Cwd               string            `json:"cwd,omitempty"`
	Stdin             string            `json:"stdin,omitempty"`
	CredentialEnvKeys []string          `json:"credential_env_keys,omitempty"`
}

// SpawnResult is the captured outcome of a single subprocess invocation
// returned to the connector through the host-function read path.
type SpawnResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// argvPattern is a tokenized argv pattern with `{name}` placeholders.
// A pattern matches an incoming argv when the lengths agree and each
// token matches.
//
// A pattern token is a sequence of literal and placeholder segments.
// `--since={since}` parses to two segments: literal `--since=` then a
// placeholder. A whole-token placeholder `{name}` is a single
// placeholder segment. Matching is greedy from left to right; literal
// segments anchor the placeholder bounds.
type argvPattern struct {
	tokens [][]argvSegment
}

type argvSegment struct {
	literal       string
	isPlaceholder bool
	// name is the placeholder's bound name (e.g. "since") when
	// isPlaceholder is true. Matching ignores the name; substitution
	// (via Substitute) requires it.
	name string
}

// parseArgvPattern tokenizes a manifest argv pattern. Tokens are
// whitespace-separated. Each token is split on `{name}` placeholders;
// the resulting segments alternate literal / placeholder.
func parseArgvPattern(pat string) argvPattern {
	fields := strings.Fields(pat)
	out := argvPattern{tokens: make([][]argvSegment, 0, len(fields))}
	for _, f := range fields {
		out.tokens = append(out.tokens, splitTokenSegments(f))
	}
	return out
}

// splitTokenSegments breaks `tok` on each `{name}` placeholder. The
// returned slice alternates literal / placeholder segments. Empty
// literal segments are preserved so adjacent placeholders work.
func splitTokenSegments(tok string) []argvSegment {
	var out []argvSegment
	for {
		open := strings.Index(tok, "{")
		if open < 0 {
			if tok != "" {
				out = append(out, argvSegment{literal: tok})
			}
			return out
		}
		close := strings.Index(tok[open:], "}")
		if close < 0 {
			// Unmatched `{` is treated as a literal.
			out = append(out, argvSegment{literal: tok})
			return out
		}
		close += open
		if close == open+1 {
			// `{}` is meaningless; treat as literal.
			out = append(out, argvSegment{literal: tok[:close+1]})
			tok = tok[close+1:]
			continue
		}
		if open > 0 {
			out = append(out, argvSegment{literal: tok[:open]})
		}
		out = append(out, argvSegment{isPlaceholder: true, name: tok[open+1 : close]})
		tok = tok[close+1:]
	}
}

// Substitute fills the placeholder segments of this pattern using
// values from `args` and returns the substituted argv tokens. Returns
// a structured *Error of class capability_denied (boundary=envelope)
// when a placeholder has no matching arg.
//
// The resulting argv is whatever the pattern produced. The caller is
// responsible for re-matching the substituted argv against the
// pattern via SpawnPolicy.CheckSpawn — substitution does not bypass
// the gate.
func (p argvPattern) Substitute(args map[string]string) ([]string, *Error) {
	out := make([]string, 0, len(p.tokens))
	for _, segs := range p.tokens {
		var b strings.Builder
		for _, seg := range segs {
			if !seg.isPlaceholder {
				b.WriteString(seg.literal)
				continue
			}
			v, ok := args[seg.name]
			if !ok {
				return nil, newCapabilityDenied(
					"spawn_op: missing value for placeholder",
					map[string]any{
						"placeholder":     "{" + seg.name + "}",
						"boundary_detail": "envelope",
					})
			}
			b.WriteString(v)
		}
		out = append(out, b.String())
	}
	return out, nil
}

// matches reports whether `argv` matches this pattern. Token count
// must agree exactly; each token's segments must consume its argv
// element.
func (p argvPattern) matches(argv []string) bool {
	if len(argv) != len(p.tokens) {
		return false
	}
	for i, segs := range p.tokens {
		if !segmentsMatch(segs, argv[i]) {
			return false
		}
	}
	return true
}

// segmentsMatch reports whether `segs` consumes `s` end-to-end. The
// algorithm walks segments left-to-right; literal segments must
// appear at the current cursor (or, when preceded by a placeholder,
// anywhere ahead and the placeholder consumes the gap).
func segmentsMatch(segs []argvSegment, s string) bool {
	if len(segs) == 0 {
		return s == ""
	}
	cursor := 0
	pendingPlaceholder := false
	for i, seg := range segs {
		if seg.isPlaceholder {
			pendingPlaceholder = true
			continue
		}
		// Literal segment.
		if pendingPlaceholder {
			idx := strings.Index(s[cursor:], seg.literal)
			if idx < 0 {
				return false
			}
			cursor += idx + len(seg.literal)
			pendingPlaceholder = false
			continue
		}
		// No placeholder pending: literal must match at cursor.
		if !strings.HasPrefix(s[cursor:], seg.literal) {
			return false
		}
		cursor += len(seg.literal)
		_ = i
	}
	if pendingPlaceholder {
		// Trailing placeholder consumes the remainder. Empty
		// remainder is also acceptable when the placeholder name is
		// non-empty (matches an empty interpolated arg).
		return true
	}
	return cursor == len(s)
}

// CheckSpawn validates an incoming SpawnEnvelope against the connector
// manifest's [capabilities.spawn] declaration. Returns a structured
// *Error of class capability_denied (boundary=sandbox) when the
// envelope falls outside the grant.
//
// The check is the first gate; the action-boundary subset (when
// supplied) and the platform sandbox apply additional enforcement.
// Mirrors HostPolicy.CheckURL's role for network capabilities.
func (p *SpawnPolicy) CheckSpawn(env SpawnEnvelope) error {
	if p == nil || len(p.programs) == 0 {
		return newCapabilityDenied(
			"spawn: connector did not declare [capabilities.spawn]",
			map[string]any{
				"requested":       "spawn:" + env.Program,
				"boundary_detail": "connector_manifest",
			})
	}
	if env.Program == "" {
		return newCapabilityDenied("spawn: program is required",
			map[string]any{"boundary_detail": "envelope"})
	}
	if _, ok := p.programs[env.Program]; !ok {
		granted := make([]string, 0, len(p.programs))
		for path := range p.programs {
			granted = append(granted, path)
		}
		return newCapabilityDenied(
			"spawn: program not in declared grant",
			map[string]any{
				"requested":       "spawn:" + env.Program,
				"granted":         prefixed(granted, "spawn:"),
				"boundary_detail": "connector_manifest",
			})
	}
	if len(env.Argv) == 0 {
		return newCapabilityDenied("spawn: argv is required",
			map[string]any{"boundary_detail": "envelope"})
	}
	if !p.argvMatches(env.Argv) {
		return newCapabilityDenied(
			"spawn: argv does not match any declared pattern",
			map[string]any{
				"requested":       env.Argv,
				"boundary_detail": "connector_manifest",
			})
	}
	for k := range env.Env {
		if _, ok := p.envAllowed[k]; !ok {
			return newCapabilityDenied(
				"spawn: env key not in declared passthrough",
				map[string]any{
					"requested":       "env:" + k,
					"boundary_detail": "connector_manifest",
				})
		}
	}
	for _, k := range env.CredentialEnvKeys {
		if _, ok := p.envAllowed[k]; !ok {
			return newCapabilityDenied(
				"spawn: credential env key not in declared passthrough",
				map[string]any{
					"requested":       "env:" + k,
					"boundary_detail": "connector_manifest",
				})
		}
	}
	if env.Cwd != "" {
		if p.cwd != "" && env.Cwd != p.cwd {
			return newCapabilityDenied(
				"spawn: cwd does not match declared policy",
				map[string]any{
					"requested":       env.Cwd,
					"declared":        p.cwd,
					"boundary_detail": "connector_manifest",
				})
		}
		if !p.cwdWithinReadScope(env.Cwd) {
			return newCapabilityDenied(
				"spawn: cwd is outside declared fs_read",
				map[string]any{
					"requested":       env.Cwd,
					"boundary_detail": "connector_manifest",
				})
		}
	}
	return nil
}

// argvMatches reports whether at least one declared argv pattern
// accepts the incoming argv.
func (p *SpawnPolicy) argvMatches(argv []string) bool {
	for _, pat := range p.argvPatterns {
		if pat.matches(argv) {
			return true
		}
	}
	return false
}

// cwdWithinReadScope reports whether `cwd` falls under any declared
// fs_read scope. A scope of `~/code/` covers `~/code/aileron`; a scope
// of `/var/spool/` covers `/var/spool/jobs`.
func (p *SpawnPolicy) cwdWithinReadScope(cwd string) bool {
	if len(p.fsRead) == 0 {
		// No FS read scope declared; cwd cannot be outside what isn't
		// declared, so accept.
		return true
	}
	for _, scope := range p.fsRead {
		if pathWithin(cwd, scope) {
			return true
		}
	}
	return false
}

// pathWithin reports whether `p` is the same as `scope` or lives under
// it. Comparison is string-based on the manifest's declared form.
// Trailing slash on `scope` is normalized so `~/code` and `~/code/`
// are equivalent prefixes.
func pathWithin(p, scope string) bool {
	scope = strings.TrimRight(scope, "/")
	if p == scope {
		return true
	}
	return strings.HasPrefix(p, scope+"/")
}

// SpawnExecutor is the narrow surface the host functions use to
// actually invoke a subprocess. Tests substitute fakes; the production
// implementation forks an os/exec.Cmd with the platform's sandbox
// applied (per ADR-0014).
//
// The executor is invoked only after CheckSpawn has approved. It is
// responsible for the second-line enforcement (FS scoping, network
// denial, process scoping) on the platforms it supports, and for
// honoring the runtime-supplied [SpawnLimits] when capturing the
// subprocess's stdout and stderr. On unsupported platforms it
// returns ErrSpawnUnavailable so the host function emits a
// structured spawn_sandbox_unavailable error.
type SpawnExecutor interface {
	Spawn(ctx context.Context, env SpawnEnvelope, limits SpawnLimits) (SpawnResult, error)
}

// ErrSpawnUnavailable is returned by SpawnExecutor.Spawn when the
// running platform does not support the spawn primitive's enforcement
// requirements. The host function translates this into a structured
// spawn_sandbox_unavailable error class.
var ErrSpawnUnavailable = errors.New("spawn: sandbox unavailable on this platform")

// defaultSpawnExecutor is the production executor. It builds an
// os/exec.Cmd with a bounded env, the manifest's cwd, and applies the
// platform-specific sandbox in applyPlatformSandbox (build-tagged per
// OS). Network denial is the platform sandbox's responsibility; the
// executor's structural floor is the bounded env and cwd.
type defaultSpawnExecutor struct{}

// Spawn implements SpawnExecutor.
func (defaultSpawnExecutor) Spawn(ctx context.Context, env SpawnEnvelope, limits SpawnLimits) (SpawnResult, error) {
	if len(env.Argv) == 0 {
		return SpawnResult{}, fmt.Errorf("spawn: empty argv")
	}
	cmd := exec.CommandContext(ctx, env.Program, env.Argv[1:]...)
	cmd.Env = make([]string, 0, len(env.Env))
	for k, v := range env.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if env.Cwd != "" {
		expanded, err := expandTilde(env.Cwd)
		if err != nil {
			return SpawnResult{}, fmt.Errorf("spawn: expand cwd: %w", err)
		}
		cmd.Dir = expanded
	}
	if env.Stdin != "" {
		cmd.Stdin = strings.NewReader(env.Stdin)
	}
	if err := applyPlatformSandbox(cmd, env); err != nil {
		return SpawnResult{}, err
	}
	stdout, stderr, runErr := runCaptured(cmd, limits)
	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return SpawnResult{
				ExitCode: -1,
				Stdout:   stdout,
				Stderr:   stderr,
			}, runErr
		}
	}
	return SpawnResult{
		ExitCode: exitCode,
		Stdout:   stdout,
		Stderr:   stderr,
	}, nil
}

// runCaptured runs `cmd` with stdout and stderr captured into capped
// byte buffers. Returns the captured bytes plus any non-Exit error.
// Output beyond the configured cap is dropped at the writer; a
// truncation marker is appended to the returned slice when a stream
// exceeded its cap.
func runCaptured(cmd *exec.Cmd, limits SpawnLimits) ([]byte, []byte, error) {
	stdout := newCaptureBuf(limits.MaxStdoutBytes)
	stderr := newCaptureBuf(limits.MaxStderrBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	return stdout.Captured(), stderr.Captured(), err
}

// captureBuf is a bounded io.Writer with a thread-safe accessor.
// os/exec writes from a goroutine in CommandContext; the buffer is
// only read after Wait returns, but we still synchronize to satisfy
// the race detector.
//
// captureBuf caps the bytes it stores at `cap`. Writes past `cap`
// are accepted (so the subprocess sees no I/O error and continues
// normally) but the trailing bytes are dropped. When at least one
// byte was dropped, Captured() appends a structured truncation
// marker to the returned slice.
//
// A cap <= 0 is treated as unbounded for tests; production code
// constructs the buffer through [newCaptureBuf] which sanitizes the
// cap to a positive value.
type captureBuf struct {
	mu       sync.Mutex
	buf      []byte
	cap      int64
	dropped  int64 // count of bytes received past the cap
}

// newCaptureBuf returns a capture buffer with the given byte cap. A
// non-positive cap falls back to [cstore.DefaultMaxStdoutBytes] as a
// defensive default; callers should pass a sane positive value from
// the resolved manifest limits.
func newCaptureBuf(cap int64) *captureBuf {
	if cap <= 0 {
		cap = cstore.DefaultMaxStdoutBytes
	}
	return &captureBuf{cap: cap}
}

func (c *captureBuf) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	remaining := c.cap - int64(len(c.buf))
	if remaining <= 0 {
		c.dropped += int64(len(p))
		return len(p), nil
	}
	if int64(len(p)) <= remaining {
		c.buf = append(c.buf, p...)
		return len(p), nil
	}
	c.buf = append(c.buf, p[:remaining]...)
	c.dropped += int64(len(p)) - remaining
	return len(p), nil
}

// Captured returns a copy of the captured bytes with a structured
// truncation marker appended when at least one byte was dropped past
// the cap. The marker is part of the returned slice so downstream
// consumers see truncation even when they cannot inspect the buffer's
// internal state.
func (c *captureBuf) Captured() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]byte, 0, len(c.buf)+truncationMarkerMaxLen)
	out = append(out, c.buf...)
	if c.dropped > 0 {
		out = append(out, fmt.Sprintf("\n[aileron: output truncated, %d bytes dropped]\n", c.dropped)...)
	}
	return out
}

// truncationMarkerMaxLen is a hint at the marker's worst-case length
// so the initial allocation in Captured fits without a reallocation
// for typical drop counts. Not a hard bound.
const truncationMarkerMaxLen = 64

// expandTilde resolves a leading `~/` or bare `~` against the user's
// home directory. Absolute paths are returned unchanged.
func expandTilde(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if p == "~" {
			return home, nil
		}
		return home + p[1:], nil
	}
	return p, nil
}

// hostSpawn implements `aileron_host.spawn(req_ptr, req_len) -> i32`.
//
// Returns:
//
//	 0  on success (output captured into per-invocation state)
//	-1  on capability_denied or platform-sandbox refusal
//	-2  on a malformed envelope
//	-3  on spawn_sandbox_unavailable
//
// On any non-zero return, a structured *Error is stuck on
// state.spawnErr so [Connector.Invoke] surfaces it instead of the
// generic connector_runtime_error path.
func hostSpawn(ctx context.Context, mod api.Module, reqPtr, reqLen uint32) int32 {
	s := stateFromCtx(ctx)
	if s == nil {
		return -1
	}
	raw := readMemory(mod, reqPtr, reqLen)
	if raw == nil {
		s.mu.Lock()
		s.spawnErr = newConnectorRuntimeError("spawn: invalid memory range")
		s.mu.Unlock()
		return -2
	}
	return processSpawn(ctx, s, raw)
}

// hostSpawnOp implements `aileron_host.spawn_op(op_ptr, op_len, args_ptr, args_len) -> i32`.
//
// op is a UTF-8 string naming an operation declared in the manifest's
// [capabilities.spawn.operations] table. args is a JSON object mapping
// placeholder names to substitution values.
//
// The runtime resolves the op against the manifest, substitutes
// placeholders, builds the spawn envelope from the manifest's program
// and env_passthrough, then dispatches through the same gate / executor
// / audit path as `spawn`. The forwarder WASM is the canonical caller;
// the function is the high-level convenience for connectors that
// don't need direct envelope control.
//
// Return codes match `spawn`:
//
//	 0  on success
//	-1  on capability_denied
//	-2  on a malformed request (bad JSON, invalid memory)
//	-3  on spawn_sandbox_unavailable
func hostSpawnOp(ctx context.Context, mod api.Module, opPtr, opLen, argsPtr, argsLen uint32) int32 {
	s := stateFromCtx(ctx)
	if s == nil {
		return -1
	}
	opBytes := readMemory(mod, opPtr, opLen)
	if opBytes == nil && opLen > 0 {
		s.mu.Lock()
		s.spawnErr = newConnectorRuntimeError("spawn_op: invalid op memory range")
		s.mu.Unlock()
		return -2
	}
	argsBytes := readMemory(mod, argsPtr, argsLen)
	if argsBytes == nil && argsLen > 0 {
		s.mu.Lock()
		s.spawnErr = newConnectorRuntimeError("spawn_op: invalid args memory range")
		s.mu.Unlock()
		return -2
	}
	return processSpawnOp(ctx, s, string(opBytes), argsBytes)
}

// processSpawnOp is the memory-independent core of hostSpawnOp. Exposed
// for tests so we can exercise the op-lookup + substitution + dispatch
// path without going through wazero linear memory.
func processSpawnOp(ctx context.Context, s *hostState, opName string, argsRaw []byte) int32 {
	var args map[string]string
	if len(argsRaw) > 0 {
		if err := json.Unmarshal(argsRaw, &args); err != nil {
			s.mu.Lock()
			s.spawnErr = newConnectorRuntimeError(fmt.Sprintf("spawn_op: invalid JSON args: %s", err.Error()))
			s.mu.Unlock()
			return -2
		}
	}
	if s.spawnPolicy == nil {
		s.mu.Lock()
		s.spawnErr = newCapabilityDenied(
			"spawn_op: connector did not declare [capabilities.spawn]",
			map[string]any{
				"requested":       "spawn_op:" + opName,
				"connector":       s.connectorFQN,
				"boundary_detail": "connector_manifest",
			})
		s.mu.Unlock()
		return -1
	}
	envelope, denyErr := s.spawnPolicy.BuildEnvelopeFromOp(opName, args, os.Getenv)
	if denyErr != nil {
		s.mu.Lock()
		s.spawnErr = denyErr
		s.mu.Unlock()
		return -1
	}
	rawEnvelope, err := json.Marshal(envelope)
	if err != nil {
		s.mu.Lock()
		s.spawnErr = newConnectorRuntimeError(fmt.Sprintf("spawn_op: marshal envelope: %s", err.Error()))
		s.mu.Unlock()
		return -2
	}
	return processSpawn(ctx, s, rawEnvelope)
}

// processSpawn is the memory-independent core of hostSpawn: it parses
// the envelope, runs the gates, hands to the executor, captures
// output, and emits audit. Exposed for tests so we can exercise the
// gate and the host-function plumbing without going through wazero
// linear memory.
func processSpawn(ctx context.Context, s *hostState, raw []byte) int32 {
	var env SpawnEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		s.mu.Lock()
		s.spawnErr = newConnectorRuntimeError(fmt.Sprintf("spawn: invalid JSON envelope: %s", err.Error()))
		s.mu.Unlock()
		return -2
	}
	if s.spawnPolicy == nil {
		s.mu.Lock()
		s.spawnErr = newCapabilityDenied(
			"spawn: connector did not declare [capabilities.spawn]",
			map[string]any{
				"requested":       "spawn:" + env.Program,
				"connector":       s.connectorFQN,
				"boundary_detail": "connector_manifest",
			})
		s.mu.Unlock()
		emitSpawnAudit(ctx, s, env, "deny", -1, nil, nil, "capability_denied")
		return -1
	}

	// First gate: connector manifest.
	if err := s.spawnPolicy.CheckSpawn(env); err != nil {
		s.mu.Lock()
		s.spawnErr = err.(*Error)
		s.mu.Unlock()
		emitSpawnAudit(ctx, s, env, "deny", -1, nil, nil, "capability_denied")
		return -1
	}

	// Second gate: action-boundary subset. The action declares which
	// programs it allows the connector to spawn. When AllowedSpawnPrograms
	// is non-empty, env.Program must appear in that subset; empty means
	// the action did not narrow the connector's grant and the manifest
	// gate above is the only check.
	if len(s.actionSpawnGrant) > 0 {
		if _, ok := s.actionSpawnGrant[env.Program]; !ok {
			granted := make([]string, 0, len(s.actionSpawnGrant))
			for k := range s.actionSpawnGrant {
				granted = append(granted, k)
			}
			s.mu.Lock()
			s.spawnErr = &Error{
				Class:    ClassCapabilityDenied,
				Boundary: BoundarySandbox,
				Message:  "spawn: program not in action's declared capability subset",
				Details: map[string]any{
					"requested":       "spawn:" + env.Program,
					"action_subset":   prefixed(granted, "spawn:"),
					"boundary_detail": "action",
				},
			}
			s.mu.Unlock()
			emitSpawnAudit(ctx, s, env, "deny", -1, nil, nil, "capability_denied")
			return -1
		}
	}

	// Credential injection: never expose vault bytes to the connector.
	if len(env.CredentialEnvKeys) > 0 {
		injected, denyErr := injectSpawnCredentials(ctx, s, env)
		if denyErr != nil {
			s.mu.Lock()
			s.spawnErr = denyErr
			s.mu.Unlock()
			emitSpawnAudit(ctx, s, env, "deny", -1, nil, nil, string(denyErr.Class))
			return -1
		}
		if env.Env == nil {
			env.Env = map[string]string{}
		}
		for k, v := range injected {
			env.Env[k] = v
		}
	}

	// Network proxy: when the connector declares [capabilities.network],
	// the runtime stands up a per-spawn CONNECT proxy on host loopback
	// and injects HTTPS_PROXY/HTTP_PROXY into the subprocess's env
	// (per ADR-0014's "Network confinement: daemon-mediated proxy").
	// The allowlist match logic lives in s.policy, reused from the WASM
	// HTTP gate. The proxy goroutine is torn down when this function
	// returns. Connectors that omit [capabilities.network] get no
	// proxy and no HTTPS_PROXY env; the platform sandbox denies all
	// outbound at the kernel boundary.
	if s.policy != nil && len(s.policy.AllowedHosts()) > 0 {
		proxy, proxyAddr, proxyClose, err := startSpawnProxy(ctx, s.policy, s.logger, s.connectorFQN)
		if err != nil {
			s.mu.Lock()
			s.spawnErr = newConnectorRuntimeError(fmt.Sprintf("spawn: start network proxy: %s", err.Error()))
			s.mu.Unlock()
			emitSpawnAudit(ctx, s, env, "error", -1, nil, nil, "connector_runtime_error")
			return -1
		}
		_ = proxy // retain for future per-spawn audit context attach
		defer proxyClose()
		if env.Env == nil {
			env.Env = map[string]string{}
		}
		env.Env["HTTPS_PROXY"] = "http://" + proxyAddr
		env.Env["HTTP_PROXY"] = "http://" + proxyAddr
	}

	// Hand to the platform executor. Sandbox unavailability is
	// translated into a distinct error class so operators can tell
	// "rules were broken" from "platform cannot enforce rules".
	executor := s.spawnExecutor
	if executor == nil {
		executor = defaultSpawnExecutor{}
	}
	limits := SpawnLimits{
		MaxStdoutBytes: s.spawnPolicy.stdoutCap,
		MaxStderrBytes: s.spawnPolicy.stderrCap,
	}
	res, runErr := executor.Spawn(ctx, env, limits)
	if runErr != nil {
		if errors.Is(runErr, ErrSpawnUnavailable) {
			s.mu.Lock()
			s.spawnErr = newSpawnSandboxUnavailable(env.Program, runErr.Error())
			s.mu.Unlock()
			emitSpawnAudit(ctx, s, env, "deny", -1, nil, nil, "spawn_sandbox_unavailable")
			return -3
		}
		s.mu.Lock()
		s.spawnErr = newConnectorRuntimeError(fmt.Sprintf("spawn: %s", runErr.Error()))
		s.mu.Unlock()
		emitSpawnAudit(ctx, s, env, "error", res.ExitCode, res.Stdout, res.Stderr, "connector_runtime_error")
		return -1
	}

	s.mu.Lock()
	s.spawnExitCode = res.ExitCode
	s.spawnStdout = res.Stdout
	s.spawnStderr = res.Stderr
	s.spawnErr = nil
	s.spawnHasResult = true
	s.mu.Unlock()
	emitSpawnAudit(ctx, s, env, "allow", res.ExitCode, res.Stdout, res.Stderr, "")
	return 0
}

// hostSpawnStatus implements `aileron_host.spawn_status() -> i32`.
// Returns the most recent subprocess's exit code; -2 when no spawn
// has been invoked yet on this connector instance. Real exit codes
// (including negative ones from signal-killed processes) are not
// confused with -2 because the connector must call spawn before
// status, and a successful spawn sets spawnHasResult.
func hostSpawnStatus(ctx context.Context) int32 {
	s := stateFromCtx(ctx)
	if s == nil {
		return -1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.spawnHasResult {
		return -2
	}
	return int32(s.spawnExitCode)
}

// hostSpawnOutputSize implements
// `aileron_host.spawn_output_size(which) -> i32`. `which`=0 reports
// stdout size; `which`=1 reports stderr size. Returns -1 when no
// subprocess output is buffered.
func hostSpawnOutputSize(ctx context.Context, which uint32) int32 {
	s := stateFromCtx(ctx)
	if s == nil {
		return -1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch which {
	case 0:
		if s.spawnStdout == nil {
			return -1
		}
		return int32(len(s.spawnStdout))
	case 1:
		if s.spawnStderr == nil {
			return -1
		}
		return int32(len(s.spawnStderr))
	default:
		return -1
	}
}

// hostSpawnOutputRead implements
// `aileron_host.spawn_output_read(which, dst_ptr, dst_len) -> i32`.
// Copies up to `dstLen` bytes of the named output (0=stdout, 1=stderr)
// into module memory at `dstPtr`. Returns the number of bytes written;
// -1 when no output is buffered or the destination range is invalid.
func hostSpawnOutputRead(ctx context.Context, mod api.Module, which, dstPtr, dstLen uint32) int32 {
	s := stateFromCtx(ctx)
	if s == nil {
		return -1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var src []byte
	switch which {
	case 0:
		src = s.spawnStdout
	case 1:
		src = s.spawnStderr
	default:
		return -1
	}
	if src == nil {
		return -1
	}
	n := uint32(len(src))
	if n > dstLen {
		n = dstLen
	}
	if n == 0 {
		return 0
	}
	wrote := writeMemory(mod, dstPtr, src[:n])
	if wrote == 0 {
		return -1
	}
	return int32(wrote)
}

// injectSpawnCredentials resolves the connector's bound credential and
// returns the env values to set for each requested credential env key.
// Mirrors injectCredential's role for HTTP requests, with two
// differences:
//
//  1. The credential is delivered as an environment variable on the
//     subprocess rather than an HTTP Authorization header.
//  2. Each entry in env.CredentialEnvKeys gets the same credential
//     value; the connector composes which env key the subprocess
//     expects (e.g. `GH_TOKEN` for the gh CLI, `SLACK_TOKEN` for
//     slackdump).
//
// The credential bytes never leave this function except as the env
// value the runtime sets on the os/exec.Cmd. The connector never
// observes them.
func injectSpawnCredentials(ctx context.Context, s *hostState, env SpawnEnvelope) (map[string]string, *Error) {
	if s.expectedCredentialKind == "" {
		return nil, newCapabilityDenied(
			"spawn: connector did not declare [capabilities.credential]",
			map[string]any{
				"requested":       "credential_env",
				"connector":       s.connectorFQN,
				"boundary_detail": "connector_manifest",
			})
	}
	if s.credentialResolver == nil {
		return nil, newBindingRequired(
			fmt.Sprintf(
				"spawn: no credential binding found for connector %s (kind: %s); run `aileron binding setup %s` to create one",
				s.connectorFQN, s.expectedCredentialKind, s.connectorFQN),
			map[string]any{
				"connector":       s.connectorFQN,
				"capability_kind": s.expectedCredentialKind,
				"boundary_detail": "action",
			})
	}
	cred, resolveErr := s.credentialResolver.Resolve(ctx)
	if resolveErr != nil {
		switch {
		case errors.Is(resolveErr, credential.ErrBindingMissing),
			errors.Is(resolveErr, credential.ErrNoBindingResolver):
			return nil, newBindingRequired(
				resolveErr.Error(),
				map[string]any{
					"connector":       s.connectorFQN,
					"capability_kind": s.expectedCredentialKind,
					"boundary_detail": "action",
				})
		case errors.Is(resolveErr, credential.ErrCredentialKindMismatch):
			return nil, newCapabilityDenied(
				resolveErr.Error(),
				map[string]any{
					"connector":       s.connectorFQN,
					"capability_kind": s.expectedCredentialKind,
					"boundary_detail": "vault",
				})
		default:
			return nil, newConnectorRuntimeError(fmt.Sprintf(
				"spawn: resolve credential: %s", resolveErr.Error()))
		}
	}
	out := make(map[string]string, len(env.CredentialEnvKeys))
	for _, k := range env.CredentialEnvKeys {
		out[k] = string(cred.Value)
	}
	return out, nil
}

// newSpawnSandboxUnavailable builds the spawn_sandbox_unavailable
// error used when the platform cannot enforce the manifest's bounds.
// Distinct from capability_denied: the rules were not broken by the
// connector, the platform cannot enforce them. Operators read the
// reason field to remediate (e.g. enable unprivileged user namespaces
// on Linux).
func newSpawnSandboxUnavailable(program, reason string) *Error {
	return &Error{
		Class:    "spawn_sandbox_unavailable",
		Message:  "spawn: sandbox unavailable on this platform",
		Boundary: BoundarySandbox,
		Details: map[string]any{
			"program": program,
			"reason":  reason,
		},
	}
}

// emitSpawnAudit logs a structured audit event for one spawn
// invocation. Captures connector identity, program, argv pattern (not
// interpolated arguments, which may be sensitive), exit code, decision,
// and content hashes of stdout and stderr.
//
// Uses audit.AttrPolicyDecision per the reservation in #480.
func emitSpawnAudit(ctx context.Context, s *hostState, env SpawnEnvelope, decision string, exit int, stdout, stderr []byte, denyClass string) {
	if s == nil || s.logger == nil {
		return
	}
	attrs := []slog.Attr{
		slog.String("connector", s.connectorFQN),
		slog.String("program", env.Program),
		slog.String("argv_shape", argvShape(env.Argv)),
		slog.String(audit.AttrPolicyDecision, decision),
		slog.Int("exit_code", exit),
	}
	if denyClass != "" {
		attrs = append(attrs, slog.String("deny_class", denyClass))
	}
	if len(stdout) > 0 {
		attrs = append(attrs, slog.String("stdout_sha256", hashHex(stdout)))
		attrs = append(attrs, slog.Int("stdout_bytes", len(stdout)))
	}
	if len(stderr) > 0 {
		attrs = append(attrs, slog.String("stderr_sha256", hashHex(stderr)))
		attrs = append(attrs, slog.Int("stderr_bytes", len(stderr)))
	}
	s.logger.LogAttrs(ctx, slog.LevelInfo, "spawn", attrs...)
}

// argvShape produces a shape-only summary of an argv: argv[0] plus a
// fixed-length tail summary (token count and a hash of the
// remainder). Avoids leaking interpolated arguments while still giving
// auditors a stable per-invocation correlation key.
func argvShape(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	if len(argv) == 1 {
		return argv[0]
	}
	rest := strings.Join(argv[1:], " ")
	return fmt.Sprintf("%s[%d:%s]", argv[0], len(argv)-1, hashHex([]byte(rest))[:16])
}

// hashHex returns the hex-encoded SHA-256 of `b`.
func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// WithSpawnExecutor overrides the executor the runtime uses for
// `aileron_host.spawn`. Defaults to the production
// defaultSpawnExecutor; tests substitute fake executors to exercise
// the gate and the host-function plumbing without forking real
// processes.
func WithSpawnExecutor(e SpawnExecutor) RuntimeOption {
	return func(r *WazeroRuntime) { r.spawnExecutor = e }
}
