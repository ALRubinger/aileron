package audit

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// TraceNodeKind distinguishes the roles a node plays in a provenance
// graph. It mirrors the webapp's provenance model
// (`webapp/src/lib/audit/provenance.ts`) so a client can consume the
// server-assembled graph without reshaping.
type TraceNodeKind string

const (
	// TraceNodeArtifact is a materialized output (root or upstream).
	TraceNodeArtifact TraceNodeKind = "artifact"
	// TraceNodeStep is the step that materialized an artifact.
	TraceNodeStep TraceNodeKind = "step"
	// TraceNodeLiteral is a terminal input leaf carrying no content hash.
	TraceNodeLiteral TraceNodeKind = "literal"
	// TraceNodePlanInput is a terminal input leaf that carries a content
	// hash but is a plan-provided root input (source `inputs.*`), not a
	// `steps.*` lineage edge. It legitimately has a hash and no producing
	// record, so it is a resolved leaf (Dangling=false), never the red
	// "(unresolved artifact)" node.
	TraceNodePlanInput TraceNodeKind = "plan_input"
	// TraceNodeLaunch is the terminal plan/actor node.
	TraceNodeLaunch TraceNodeKind = "launch"
)

// TraceLiteral describes the input binding/source a literal leaf stands
// for. A literal input has no backing audit record.
type TraceLiteral struct {
	Binding string
	Source  string
}

// TraceNode is one node in an assembled provenance graph.
type TraceNode struct {
	// ID is a stable id, unique within the graph.
	ID string
	// Kind is the node's role in the graph.
	Kind TraceNodeKind
	// Title is a one-line summary the consumer renders.
	Title string
	// Subtitle is an optional secondary detail line.
	Subtitle string
	// Depth is the distance from the root artifact (0 = root).
	Depth int
	// Event is the backing audit event, when the node was derived from
	// one. A literal input has no backing record.
	Event *Event
	// ContentHash is, for an artifact node, the content hash it
	// materialized.
	ContentHash string
	// Dangling is true when an artifact node's content hash could not be
	// resolved to a producing record (dangling upstream).
	Dangling bool
	// Literal, for a literal node, carries the input binding/source it
	// stands for.
	Literal *TraceLiteral
}

// TraceEdge is a directed edge running parent → child (downstream
// consumer → upstream producer).
type TraceEdge struct {
	From string
	To   string
}

// TraceGraph is the assembled provenance graph for a root artifact (or
// launch).
type TraceGraph struct {
	RootID string
	Nodes  []TraceNode
	Edges  []TraceEdge
}

// TraceRoot selects the starting point of a provenance walk. At least
// one of ContentHash or InvocationID must be set; ContentHash takes
// precedence when both are supplied.
type TraceRoot struct {
	// ContentHash roots the walk at the single `output.materialized`
	// event carrying this content hash (`sha256:<hex>`).
	ContentHash string
	// InvocationID roots the walk at every `output.materialized` event
	// carrying this launch-scoped id. Used when ContentHash is empty.
	InvocationID string
}

// ErrTraceRootNotFound is returned by [AssembleTrace] when the requested
// root (content hash or invocation) resolves to no `output.materialized`
// record. The handler maps this to a 404.
var ErrTraceRootNotFound = errors.New("audit: trace root not found")

// ErrTraceRootUnspecified is returned by [AssembleTrace] when neither a
// content hash nor an invocation id was supplied. The handler maps this
// to a 400.
var ErrTraceRootUnspecified = errors.New("audit: trace root requires content_hash or invocation_id")

// maxTraceDepth caps the walk to mirror the webapp's MAX_DEPTH guard,
// bounding a pathological (or maliciously deep) provenance chain.
const maxTraceDepth = 32

// materializedEventType is the event type the runtime stamps on each
// per-output provenance record. The walk roots and recurses only on
// these records.
const materializedEventType = "output.materialized"

// eventLister is the read surface [AssembleTrace] needs: the flat-event
// query used to resolve a content hash to its producing record and to
// enumerate a launch's records. [EventStore] satisfies it, so both the
// in-memory and file-backed stores work unchanged.
type eventLister interface {
	ListEvents(ctx context.Context, f EventFilter) ([]Event, error)
}

// AssembleTrace walks recorded `output.materialized` events back through
// each step's `aileron.step.inputs[]` and returns a provenance graph. It
// mirrors the webapp's `assembleProvenance` semantics exactly: an
// artifact node keyed by content hash, its materializing step, literal
// leaves for inputs without a content hash, recursion into hashed
// upstreams guarded against cycles by a seen-hash set, dangling
// upstreams marked (not fatal), a depth cap, and a terminal launch node
// derived from the root's plan/actor provenance.
//
// Rooting: when root.ContentHash is set it resolves the single producing
// event and walks back from it. Otherwise root.InvocationID enumerates
// that launch's `output.materialized` events and assembles each terminal
// output into one shared graph (an output consumed by another output in
// the same launch is reached as an upstream, not re-walked as a root).
//
// The traversal goes through the [eventLister] read surface only; it
// never mutates the store.
func AssembleTrace(ctx context.Context, store eventLister, root TraceRoot) (TraceGraph, error) {
	if store == nil {
		return TraceGraph{}, errors.New("audit: nil store")
	}
	if root.ContentHash == "" && root.InvocationID == "" {
		return TraceGraph{}, ErrTraceRootUnspecified
	}

	roots, err := resolveRoots(ctx, store, root)
	if err != nil {
		return TraceGraph{}, err
	}
	if len(roots) == 0 {
		return TraceGraph{}, ErrTraceRootNotFound
	}

	w := &traceWalker{ctx: ctx, store: store, seen: map[string]bool{}}

	// Walk each root that has not already appeared as an upstream of an
	// earlier root. The first root actually walked is the graph root; for
	// content-hash rooting there is exactly one, for invocation rooting
	// the roots arrive newest-first so the graph root is the newest
	// terminal output.
	var rootID string
	var rootArtifactIDs []string
	var launchSource Event
	for i := range roots {
		ev := roots[i]
		if h := outputContentHash(ev); h != "" && w.seen[h] {
			// Already materialized as an upstream of an earlier terminal;
			// its subgraph is present, so do not re-walk it as a root.
			continue
		}
		id := w.walk(ev, 0)
		if rootID == "" {
			rootID = id
			launchSource = ev
		}
		rootArtifactIDs = append(rootArtifactIDs, id)
	}

	// A store error encountered while resolving an upstream during the walk
	// must surface as an error, not be silently rendered as a dangling
	// node: a dangling marker means "no such record", whereas a store
	// failure means the graph is unknown. Returning the error lets the
	// handler map it to a 500 instead of a misleading partial graph.
	if w.err != nil {
		return TraceGraph{}, w.err
	}

	// Terminal launch/actor node. All roots of one launch share the same
	// plan/actor provenance, so a single launch node hangs off each root's
	// step. Placed one below the deepest node.
	if len(rootArtifactIDs) > 0 {
		maxDepth := 0
		for _, n := range w.nodes {
			if n.Depth > maxDepth {
				maxDepth = n.Depth
			}
		}
		w.nodes = append(w.nodes, launchNode(launchSource, maxDepth+1))
		for _, id := range rootArtifactIDs {
			w.edges = append(w.edges, TraceEdge{From: "step:" + id, To: "launch"})
		}
	}

	return TraceGraph{RootID: rootID, Nodes: w.nodes, Edges: w.edges}, nil
}

// resolveRoots returns the `output.materialized` events to walk from,
// newest-first. ContentHash resolves the single producing event;
// InvocationID enumerates the launch's materialized outputs.
func resolveRoots(ctx context.Context, store eventLister, root TraceRoot) ([]Event, error) {
	if root.ContentHash != "" {
		events, err := store.ListEvents(ctx, EventFilter{ContentHash: root.ContentHash, Limit: 1})
		if err != nil {
			return nil, err
		}
		return events, nil
	}
	events, err := store.ListEvents(ctx, EventFilter{InvocationID: root.InvocationID})
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(events))
	for _, e := range events {
		if string(e.EventType) == materializedEventType {
			out = append(out, e)
		}
	}
	return out, nil
}

// traceWalker holds the mutable state of one assembly: the accumulating
// nodes/edges, the seen-hash cycle guard, and a literal-leaf sequence.
type traceWalker struct {
	ctx          context.Context
	store        eventLister
	nodes        []TraceNode
	edges        []TraceEdge
	seen         map[string]bool
	literalSeq   int
	planInputSeq int
	// err holds the first store error encountered while resolving an
	// upstream. A store failure is distinct from a missing record (which
	// is a legitimate dangling upstream): the walk records it here and
	// AssembleTrace surfaces it rather than fabricating a dangling node.
	err error
}

// walk expands one artifact event: it emits the artifact node, its step
// node, the input nodes/edges, and recurses into hashed upstreams.
// It returns the id of the artifact node so callers can wire an edge to
// it. This mirrors `assembleProvenance`'s inner `walk`.
func (w *traceWalker) walk(event Event, depth int) string {
	hash := outputContentHash(event)
	var artifactID string
	if hash != "" {
		artifactID = "artifact:" + hash
	} else {
		artifactID = fmt.Sprintf("artifact:node%d", len(w.nodes))
	}

	ev := event
	w.nodes = append(w.nodes, TraceNode{
		ID:          artifactID,
		Kind:        TraceNodeArtifact,
		Title:       artifactTitle(event),
		Subtitle:    artifactSubtitle(event),
		Depth:       depth,
		Event:       &ev,
		ContentHash: hash,
	})
	if hash != "" {
		w.seen[hash] = true
	}

	// The materializing step node hangs directly off the artifact.
	sid := stepID(event)
	stepNodeID := "step:" + artifactID
	stepTitle := "Step"
	if sid != "" {
		stepTitle = "Step " + sid
	}
	w.nodes = append(w.nodes, TraceNode{
		ID:       stepNodeID,
		Kind:     TraceNodeStep,
		Title:    stepTitle,
		Subtitle: stepSubtitle(event),
		Depth:    depth + 1,
		Event:    &ev,
	})
	w.edges = append(w.edges, TraceEdge{From: artifactID, To: stepNodeID})

	if depth+2 > maxTraceDepth {
		return artifactID
	}

	for _, input := range stepInputs(event) {
		if input.ContentHash == "" {
			// Literal input: a terminal leaf, never recursed.
			litID := fmt.Sprintf("literal:%d", w.literalSeq)
			w.literalSeq++
			title := input.Binding
			if title == "" {
				title = "(literal)"
			}
			w.nodes = append(w.nodes, TraceNode{
				ID:       litID,
				Kind:     TraceNodeLiteral,
				Title:    title,
				Subtitle: input.Source,
				Depth:    depth + 2,
				Literal:  &TraceLiteral{Binding: input.Binding, Source: input.Source},
			})
			w.edges = append(w.edges, TraceEdge{From: stepNodeID, To: litID})
			continue
		}

		if !strings.HasPrefix(input.Source, "steps.") {
			// Plan-provided root input: a hashed input whose source is not
			// a `steps.*` lineage edge (e.g. `inputs.region`). It has a
			// content hash but no producing `output.materialized` record by
			// design, so it is a resolved terminal leaf, never the red
			// "(unresolved artifact)" dangling node. Keyed per-node like a
			// literal and not added to the cycle set: a plan input has no
			// upstream to recurse into, so it cannot participate in a cycle.
			piID := fmt.Sprintf("plan_input:%d", w.planInputSeq)
			w.planInputSeq++
			title := input.Binding
			if title == "" {
				title = "(plan input)"
			}
			w.nodes = append(w.nodes, TraceNode{
				ID:          piID,
				Kind:        TraceNodePlanInput,
				Title:       title,
				Subtitle:    shortHash(input.ContentHash),
				Depth:       depth + 2,
				ContentHash: input.ContentHash,
				Literal:     &TraceLiteral{Binding: input.Binding, Source: input.Source},
			})
			w.edges = append(w.edges, TraceEdge{From: stepNodeID, To: piID})
			continue
		}

		if w.seen[input.ContentHash] {
			// Already expanded upstream (or in-flight): wire the edge to
			// the existing artifact node and do not recurse (cycle guard).
			w.edges = append(w.edges, TraceEdge{From: stepNodeID, To: "artifact:" + input.ContentHash})
			continue
		}
		// Reserve the hash before resolving so a cyclic path cannot
		// re-enter it.
		w.seen[input.ContentHash] = true

		upstream, ok := w.resolve(input.ContentHash)
		if !ok {
			// Dangling upstream: mark it and stop, do not fail.
			danglingID := "artifact:" + input.ContentHash
			w.nodes = append(w.nodes, TraceNode{
				ID:          danglingID,
				Kind:        TraceNodeArtifact,
				Title:       "(unresolved artifact)",
				Subtitle:    shortHash(input.ContentHash),
				Depth:       depth + 2,
				ContentHash: input.ContentHash,
				Dangling:    true,
			})
			w.edges = append(w.edges, TraceEdge{From: stepNodeID, To: danglingID})
			continue
		}

		upstreamID := w.walk(upstream, depth+2)
		w.edges = append(w.edges, TraceEdge{From: stepNodeID, To: upstreamID})
	}

	return artifactID
}

// resolve returns the `output.materialized` event that produced the
// given content hash, or ok=false when none is recorded (a legitimate
// dangling upstream). A store error is recorded on the walker (not
// conflated with "not found") so AssembleTrace can surface it; resolve
// still returns ok=false so the in-flight walk unwinds cleanly.
func (w *traceWalker) resolve(hash string) (Event, bool) {
	events, err := w.store.ListEvents(w.ctx, EventFilter{ContentHash: hash, Limit: 1})
	if err != nil {
		if w.err == nil {
			w.err = err
		}
		return Event{}, false
	}
	if len(events) == 0 {
		return Event{}, false
	}
	return events[0], true
}

// launchNode builds the terminal launch/actor node from the root
// artifact's plan/actor provenance, mirroring provenance.ts's launchNode.
func launchNode(root Event, depth int) TraceNode {
	skill := planSkill(root)
	actor := actorLabel(root)
	var parts []string
	if skill != "" {
		parts = append(parts, "skill "+skill)
	}
	if actor != "" {
		parts = append(parts, "by "+actor)
	}
	title := "Launch"
	if skill != "" {
		title = "Launch: " + skill
	}
	ev := root
	return TraceNode{
		ID:       "launch",
		Kind:     TraceNodeLaunch,
		Title:    title,
		Subtitle: strings.Join(parts, " · "),
		Depth:    depth,
		Event:    &ev,
	}
}
