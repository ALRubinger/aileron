package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/ALRubinger/aileron/internal/model"
)

// digest computes the real `sha256:<hex>` content hash of some bytes, in
// the same digest space the runtime records under
// `aileron.output.content_hash` and each file-map input's `content_hash`
// (#1912). Tests never hand-author a hash: every hash in a fixture is a
// real digest of real bytes, so the artifact→input linkage the walk
// resolves is authentic.
func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// materializedEvent builds an `output.materialized` audit event with the
// exact flat-key payload shape the runtime's buildOutputRecord emits (see
// internal/flightplan/runtime/audit.go), replaying a captured record
// rather than hand-crafting a bespoke shape.
type materializedEvent struct {
	id           string
	name         string
	mime         string
	contentHash  string
	bytes        int
	stepID       string
	stepKind     string
	transform    string
	command      string
	inputs       []map[string]any
	invocationID string
	planSkill    string
	identity     string
}

func (m materializedEvent) toEvent() Event {
	payload := map[string]any{
		"aileron.output.name":         m.name,
		"aileron.output.mime":         m.mime,
		"aileron.output.content_hash": m.contentHash,
		"aileron.output.bytes":        m.bytes,
		"aileron.step.id":             m.stepID,
		"aileron.step.kind":           m.stepKind,
	}
	if m.transform != "" {
		payload["aileron.step.transform"] = m.transform
	}
	if m.command != "" {
		payload["aileron.step.command"] = m.command
	}
	if len(m.inputs) > 0 {
		payload["aileron.step.inputs"] = m.inputs
	}
	if m.invocationID != "" {
		payload["aileron.invocation.id"] = m.invocationID
	}
	if m.planSkill != "" {
		payload["aileron.plan.skill"] = m.planSkill
	}
	actor := model.ActorRef{Type: model.ActorTypeAgent, ID: "runtime"}
	if m.identity != "" {
		actor.IdentityLabel = m.identity
		payload["aileron.actor.identity_label"] = m.identity
	}
	return Event{
		EventID:   m.id,
		EventType: materializedEventType,
		Actor:     actor,
		Payload:   payload,
	}
}

// nodeByID indexes a graph's nodes for assertions.
func nodeByID(g TraceGraph) map[string]TraceNode {
	out := make(map[string]TraceNode, len(g.Nodes))
	for _, n := range g.Nodes {
		out[n.ID] = n
	}
	return out
}

// hasEdge reports whether the graph contains a from→to edge.
func hasEdge(g TraceGraph, from, to string) bool {
	for _, e := range g.Edges {
		if e.From == from && e.To == to {
			return true
		}
	}
	return false
}

// buildTwoStepChain records a real two-step provenance chain into a
// MemStore: step s1 materializes artifact A from a literal input, step s2
// (a transform) materializes artifact B by consuming A's content hash
// plus a literal region input. Returns the store and the two hashes.
func buildTwoStepChain(t *testing.T) (*MemStore, string, string) {
	t.Helper()
	store := NewMemStore()
	ctx := context.Background()

	rawBytes := []byte(`{"rows":[1,2,3]}`)
	hashA := digest(rawBytes)
	reportBytes := []byte("a,b\n1,2\n")
	hashB := digest(reportBytes)

	evA := materializedEvent{
		id:          "evt-a",
		name:        "raw.json",
		mime:        "application/json",
		contentHash: hashA,
		bytes:       len(rawBytes),
		stepID:      "s1",
		stepKind:    "action-call",
		inputs: []map[string]any{
			{"binding": "region", "source": "inputs.region"},
		},
		invocationID: "inv-1",
		planSkill:    "athena-report",
		identity:     "analytics@corp",
	}.toEvent()
	evB := materializedEvent{
		id:          "evt-b",
		name:        "report.csv",
		mime:        "text/csv",
		contentHash: hashB,
		bytes:       len(reportBytes),
		stepID:      "s2",
		stepKind:    "transform",
		transform:   "to-csv",
		inputs: []map[string]any{
			{"binding": "data", "source": "steps.s1.out", "content_hash": hashA},
			{"binding": "label", "source": "inputs.label"},
		},
		invocationID: "inv-1",
		planSkill:    "athena-report",
		identity:     "analytics@corp",
	}.toEvent()

	if err := store.Append(ctx, evA); err != nil {
		t.Fatalf("append A: %v", err)
	}
	if err := store.Append(ctx, evB); err != nil {
		t.Fatalf("append B: %v", err)
	}
	return store, hashA, hashB
}

func TestAssembleTrace_TwoStepChainByContentHash(t *testing.T) {
	store, hashA, hashB := buildTwoStepChain(t)

	g, err := AssembleTrace(context.Background(), store, TraceRoot{ContentHash: hashB})
	if err != nil {
		t.Fatalf("AssembleTrace: %v", err)
	}

	// Root is artifact B.
	if g.RootID != "artifact:"+hashB {
		t.Fatalf("RootID = %q, want %q", g.RootID, "artifact:"+hashB)
	}

	nodes := nodeByID(g)

	// Both artifacts present, keyed by content hash.
	artB, ok := nodes["artifact:"+hashB]
	if !ok || artB.Kind != TraceNodeArtifact || artB.ContentHash != hashB {
		t.Fatalf("artifact B node missing/wrong: %+v", artB)
	}
	if artB.Depth != 0 {
		t.Errorf("artifact B depth = %d, want 0", artB.Depth)
	}
	if artB.Title != "report.csv" {
		t.Errorf("artifact B title = %q, want report.csv", artB.Title)
	}
	artA, ok := nodes["artifact:"+hashA]
	if !ok || artA.Kind != TraceNodeArtifact || artA.Dangling {
		t.Fatalf("artifact A node missing/dangling: %+v", artA)
	}
	if artA.Depth != 2 {
		t.Errorf("artifact A depth = %d, want 2", artA.Depth)
	}

	// Step nodes hang off each artifact.
	if _, ok := nodes["step:artifact:"+hashB]; !ok {
		t.Fatal("step for B missing")
	}
	if _, ok := nodes["step:artifact:"+hashA]; !ok {
		t.Fatal("step for A missing")
	}

	// The #1912 linkage: B's consuming step resolves an edge to the
	// producing artifact A.
	if !hasEdge(g, "step:artifact:"+hashB, "artifact:"+hashA) {
		t.Error("missing edge from B's step to producing artifact A")
	}
	if !hasEdge(g, "artifact:"+hashB, "step:artifact:"+hashB) {
		t.Error("missing edge from artifact B to its step")
	}
	if !hasEdge(g, "artifact:"+hashA, "step:artifact:"+hashA) {
		t.Error("missing edge from artifact A to its step")
	}

	// Literal inputs became terminal leaves (one per step's literal input).
	var literals int
	for _, n := range g.Nodes {
		if n.Kind == TraceNodeLiteral {
			literals++
			if n.Literal == nil {
				t.Errorf("literal node %s has no Literal detail", n.ID)
			}
		}
	}
	if literals != 2 {
		t.Errorf("literal leaf count = %d, want 2", literals)
	}

	// Termination at the launch node, derived from plan/actor provenance,
	// wired off the root's step.
	launch, ok := nodes["launch"]
	if !ok || launch.Kind != TraceNodeLaunch {
		t.Fatal("launch terminal node missing")
	}
	if launch.Title != "Launch: athena-report" {
		t.Errorf("launch title = %q", launch.Title)
	}
	if !hasEdge(g, "step:artifact:"+hashB, "launch") {
		t.Error("missing edge from root step to launch")
	}
}

func TestAssembleTrace_DanglingUpstreamMarkedNotFatal(t *testing.T) {
	store := NewMemStore()
	ctx := context.Background()

	// B consumes a hash whose producing record was never recorded.
	missing := digest([]byte("upstream that was never stored"))
	reportBytes := []byte("only-b")
	hashB := digest(reportBytes)
	evB := materializedEvent{
		id:          "evt-b",
		name:        "report.csv",
		contentHash: hashB,
		bytes:       len(reportBytes),
		stepID:      "s2",
		stepKind:    "transform",
		inputs: []map[string]any{
			{"binding": "data", "source": "steps.s1.out", "content_hash": missing},
		},
		invocationID: "inv-2",
	}.toEvent()
	if err := store.Append(ctx, evB); err != nil {
		t.Fatalf("append: %v", err)
	}

	g, err := AssembleTrace(ctx, store, TraceRoot{ContentHash: hashB})
	if err != nil {
		t.Fatalf("AssembleTrace: %v", err)
	}
	nodes := nodeByID(g)
	dangling, ok := nodes["artifact:"+missing]
	if !ok {
		t.Fatal("dangling upstream node missing")
	}
	if !dangling.Dangling {
		t.Error("upstream should be marked dangling")
	}
	if dangling.Event != nil {
		t.Error("dangling node should have no backing event")
	}
	if !hasEdge(g, "step:artifact:"+hashB, "artifact:"+missing) {
		t.Error("missing edge to dangling upstream")
	}
}

func TestAssembleTrace_CycleGuard(t *testing.T) {
	store := NewMemStore()
	ctx := context.Background()

	// A ⇄ B: each records the other's content hash as an input. Real
	// digests, but a deliberately cyclic linkage — the walk must terminate.
	aBytes := []byte("artifact-a")
	bBytes := []byte("artifact-b")
	hashA := digest(aBytes)
	hashB := digest(bBytes)

	evA := materializedEvent{
		id: "evt-a", name: "a", contentHash: hashA, bytes: len(aBytes),
		stepID: "s1", stepKind: "transform",
		inputs: []map[string]any{{"binding": "b", "source": "steps.s2.out", "content_hash": hashB}},
	}.toEvent()
	evB := materializedEvent{
		id: "evt-b", name: "b", contentHash: hashB, bytes: len(bBytes),
		stepID: "s2", stepKind: "transform",
		inputs: []map[string]any{{"binding": "a", "source": "steps.s1.out", "content_hash": hashA}},
	}.toEvent()
	if err := store.Append(ctx, evA); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(ctx, evB); err != nil {
		t.Fatal(err)
	}

	g, err := AssembleTrace(ctx, store, TraceRoot{ContentHash: hashB})
	if err != nil {
		t.Fatalf("AssembleTrace: %v", err)
	}
	// Each artifact appears exactly once despite the cycle.
	counts := map[string]int{}
	for _, n := range g.Nodes {
		if n.Kind == TraceNodeArtifact {
			counts[n.ContentHash]++
		}
	}
	if counts[hashA] != 1 || counts[hashB] != 1 {
		t.Fatalf("cycle guard failed: A=%d B=%d, want 1 each", counts[hashA], counts[hashB])
	}
	// The back-edge to the already-seen artifact is still wired.
	if !hasEdge(g, "step:artifact:"+hashA, "artifact:"+hashB) {
		t.Error("missing cycle back-edge from A's step to B")
	}
}

func TestAssembleTrace_ByInvocationID(t *testing.T) {
	store, hashA, hashB := buildTwoStepChain(t)

	g, err := AssembleTrace(context.Background(), store, TraceRoot{InvocationID: "inv-1"})
	if err != nil {
		t.Fatalf("AssembleTrace: %v", err)
	}
	nodes := nodeByID(g)
	// The terminal output B roots the graph; A is reached as its upstream,
	// not re-walked as a separate root.
	if _, ok := nodes["artifact:"+hashB]; !ok {
		t.Error("artifact B missing from invocation trace")
	}
	if _, ok := nodes["artifact:"+hashA]; !ok {
		t.Error("artifact A missing from invocation trace")
	}
	// A appears once (reached as upstream, not duplicated as a root).
	var aCount int
	for _, n := range g.Nodes {
		if n.ID == "artifact:"+hashA {
			aCount++
		}
	}
	if aCount != 1 {
		t.Errorf("artifact A node count = %d, want 1", aCount)
	}
	// Exactly one launch terminal for the shared invocation.
	var launches int
	for _, n := range g.Nodes {
		if n.Kind == TraceNodeLaunch {
			launches++
		}
	}
	if launches != 1 {
		t.Errorf("launch node count = %d, want 1", launches)
	}
}

func TestAssembleTrace_UnknownRootNotFound(t *testing.T) {
	store, _, _ := buildTwoStepChain(t)

	_, err := AssembleTrace(context.Background(), store, TraceRoot{ContentHash: "sha256:deadbeef"})
	if !errors.Is(err, ErrTraceRootNotFound) {
		t.Fatalf("err = %v, want ErrTraceRootNotFound", err)
	}

	_, err = AssembleTrace(context.Background(), store, TraceRoot{InvocationID: "no-such-launch"})
	if !errors.Is(err, ErrTraceRootNotFound) {
		t.Fatalf("invocation err = %v, want ErrTraceRootNotFound", err)
	}
}

func TestAssembleTrace_RootUnspecified(t *testing.T) {
	store := NewMemStore()
	_, err := AssembleTrace(context.Background(), store, TraceRoot{})
	if !errors.Is(err, ErrTraceRootUnspecified) {
		t.Fatalf("err = %v, want ErrTraceRootUnspecified", err)
	}
}

func TestAssembleTrace_NilStore(t *testing.T) {
	_, err := AssembleTrace(context.Background(), nil, TraceRoot{ContentHash: "sha256:x"})
	if err == nil {
		t.Fatal("expected error for nil store")
	}
}

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{-1, ""},
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{10 * 1024, "10 KB"},
		{1024 * 1024, "1.0 MB"},
		{5 * 1024 * 1024 * 1024, "5.0 GB"},
		{2 * 1024 * 1024 * 1024 * 1024, "2.0 TB"},
	}
	for _, c := range cases {
		if got := formatBytes(c.in); got != c.want {
			t.Errorf("formatBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestShortHash(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"sha256:short", "sha256:short"},
		{"barehexvalue0", "barehexv…lue0"},
		{"sha256:0123456789abcdef0123", "sha256:01234567…0123"},
	}
	for _, c := range cases {
		if got := shortHash(c.in); got != c.want {
			t.Errorf("shortHash(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestStepInputs_MalformedEntriesSkipped(t *testing.T) {
	ev := Event{Payload: map[string]any{
		"aileron.step.inputs": []any{
			"not-an-object",                                       // wrong type
			map[string]any{"source": "no-binding"},                // missing binding
			map[string]any{"binding": ""},                         // empty binding
			map[string]any{"binding": "ok", "source": "inputs.x"}, // valid
		},
	}}
	got := stepInputs(ev)
	if len(got) != 1 {
		t.Fatalf("stepInputs kept %d entries, want 1", len(got))
	}
	if got[0].Binding != "ok" || got[0].Source != "inputs.x" {
		t.Errorf("kept entry = %+v", got[0])
	}
}

func TestStepInputs_NonArrayIsEmpty(t *testing.T) {
	ev := Event{Payload: map[string]any{"aileron.step.inputs": "oops"}}
	if got := stepInputs(ev); len(got) != 0 {
		t.Errorf("stepInputs on non-array = %v, want empty", got)
	}
	ev2 := Event{Payload: map[string]any{}}
	if got := stepInputs(ev2); got != nil {
		t.Errorf("stepInputs on missing key = %v, want nil", got)
	}
}

// TestAssembleTrace_DepthCap builds a deep linear chain and asserts the
// walk stops at maxTraceDepth rather than recursing without bound.
func TestAssembleTrace_DepthCap(t *testing.T) {
	store := NewMemStore()
	ctx := context.Background()

	// Build a chain of 40 artifacts, each consuming the previous one's
	// hash. The deepest hash is the leaf; the newest is the root.
	const depth = 40
	hashes := make([]string, depth)
	for i := 0; i < depth; i++ {
		hashes[i] = digest([]byte{byte(i), byte(i >> 8), 0x5a})
	}
	for i := 0; i < depth; i++ {
		me := materializedEvent{
			id:          "evt-" + hashes[i],
			name:        "artifact",
			contentHash: hashes[i],
			bytes:       10,
			stepID:      "s",
			stepKind:    "transform",
		}
		if i > 0 {
			me.inputs = []map[string]any{
				{"binding": "prev", "source": "steps.prev.out", "content_hash": hashes[i-1]},
			}
		}
		if err := store.Append(ctx, me.toEvent()); err != nil {
			t.Fatal(err)
		}
	}

	g, err := AssembleTrace(ctx, store, TraceRoot{ContentHash: hashes[depth-1]})
	if err != nil {
		t.Fatalf("AssembleTrace: %v", err)
	}
	// Artifact recursion is bounded by the depth cap: artifact nodes sit
	// at even depths and never recurse past maxTraceDepth (a step node,
	// like the terminal launch, may sit one below the deepest artifact,
	// mirroring the webapp's MAX_DEPTH semantics).
	var artifacts int
	for _, n := range g.Nodes {
		if n.Kind == TraceNodeArtifact {
			artifacts++
			if n.Depth > maxTraceDepth {
				t.Errorf("artifact %s depth %d exceeds cap %d", n.ID, n.Depth, maxTraceDepth)
			}
		}
	}
	// The walk truncated: fewer than the full 40-artifact chain was
	// expanded, proving the cap stopped unbounded recursion.
	if artifacts == 0 || artifacts >= depth {
		t.Fatalf("expected a truncated artifact set (0 < n < %d), got %d", depth, artifacts)
	}
}

func TestArtifactTitle_UnnamedFallback(t *testing.T) {
	ev := Event{Payload: map[string]any{}}
	if got := artifactTitle(ev); got != "(unnamed artifact)" {
		t.Errorf("artifactTitle = %q, want (unnamed artifact)", got)
	}
}

func TestStepSubtitle_ToolCommand(t *testing.T) {
	ev := materializedEvent{
		stepKind: "tool",
		command:  "python analyze.py",
	}.toEvent()
	got := stepSubtitle(ev)
	if got != "tool · python analyze.py" {
		t.Errorf("stepSubtitle = %q, want 'tool · python analyze.py'", got)
	}
}

// TestAssembleTrace_JSONRoundTrippedPayload proves the walk reads the
// payload shape that survives a `POST /v1/audit` ingest, where numbers
// arrive as float64 and inputs as []any of map[string]any.
func TestAssembleTrace_JSONRoundTrippedPayload(t *testing.T) {
	store := NewMemStore()
	ctx := context.Background()
	reportBytes := []byte("json-round-tripped")
	hashB := digest(reportBytes)
	ev := Event{
		EventID:   "evt-b",
		EventType: materializedEventType,
		Actor:     model.ActorRef{Type: model.ActorTypeAgent, ID: "runtime"},
		Payload: map[string]any{
			"aileron.output.name":         "report.csv",
			"aileron.output.content_hash": hashB,
			// float64, as JSON decoding produces.
			"aileron.output.bytes": float64(len(reportBytes)),
			"aileron.step.id":      "s2",
			"aileron.step.kind":    "transform",
			// []any of map[string]any, as JSON decoding produces.
			"aileron.step.inputs": []any{
				map[string]any{"binding": "region", "source": "inputs.region"},
			},
			"aileron.plan.skill": "athena-report",
		},
	}
	if err := store.Append(ctx, ev); err != nil {
		t.Fatal(err)
	}
	g, err := AssembleTrace(ctx, store, TraceRoot{ContentHash: hashB})
	if err != nil {
		t.Fatalf("AssembleTrace: %v", err)
	}
	nodes := nodeByID(g)
	art := nodes["artifact:"+hashB]
	// Byte count coerced from float64 into the subtitle.
	if art.Subtitle == "" {
		t.Error("expected subtitle with size from float64 bytes")
	}
	var literals int
	for _, n := range g.Nodes {
		if n.Kind == TraceNodeLiteral {
			literals++
		}
	}
	if literals != 1 {
		t.Errorf("literal count = %d, want 1 (from []any input)", literals)
	}
}
