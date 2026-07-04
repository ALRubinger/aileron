package runtime

import (
	"context"
	"testing"
)

// fileMapChainPlan builds a real two-step chain that exercises the file-map
// input walk-back (#1891/#1912):
//
//   - produce_a materializes a file-map artifact (a.txt) from a transform that
//     emits a {path,mimeType,encoding,content} carrier.
//   - consume_a binds produce_a's materialized file-map output AND a sibling
//     plain-data output, then materializes its own artifact (b.txt).
//
// The chain lets the test pull produce_a's producer `aileron.output.content_hash`
// and consume_a's `aileron.step.inputs[]` entries from records the runtime
// actually emitted, rather than hand-authored hashes.
func fileMapChainPlan() *Plan {
	mb := func(s string) Binding { b, _ := ParseBinding(s); return b }
	p := &Plan{
		Name: "file-map-chain",
		Outputs: map[string]Output{
			"a.txt": {Name: "a.txt", MimeType: "text/plain", Encoding: EncodingUTF8, Target: PublishFile, Path: "a.txt"},
			"b.txt": {Name: "b.txt", MimeType: "text/plain", Encoding: EncodingUTF8, Target: PublishFile, Path: "b.txt"},
		},
		Steps: []Step{
			{ID: "produce_a", Kind: KindTransform, Transform: "emit_a",
				Outputs: []string{"file"}, MaterializesOutput: "a.txt"},
			{ID: "produce_data", Kind: KindTransform, Transform: "emit_data",
				Outputs: []string{"data"}},
			{ID: "consume_a", Kind: KindTransform, Transform: "wrap_b",
				Bindings: map[string]Binding{
					"upstream": mb("steps.produce_a.file"),
					"meta":     mb("steps.produce_data.data"),
				},
				Outputs: []string{"file"}, MaterializesOutput: "b.txt"},
		},
	}
	order, err := topoSort(p, map[string]int{"produce_a": 0, "produce_data": 1, "consume_a": 2})
	if err != nil {
		panic(err)
	}
	p.Order = order
	return p
}

// aContent is produce_a's materialized file-map content — the exact bytes the
// producer digests into aileron.output.content_hash and, post-fix, the bytes the
// downstream input walk-back must digest to link back.
const aContent = "alpha metrics\ncpu,42\n"

// metaValue is the sibling plain-data output consume_a also binds; it has no
// `content` key, so it stays on the canonicalValueDigest branch (no regression).
var metaValue = map[string]any{"rows": float64(2), "source": "cpu"}

func fileMapChainRegistry() *TransformRegistry {
	reg := NewTransformRegistry()
	reg.Register("emit_a", func(_ map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: map[string]any{
			"path": "a.txt", "mimeType": "text/plain", "encoding": "utf-8", "content": aContent,
		}}, nil
	})
	reg.Register("emit_data", func(_ map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: metaValue}, nil
	})
	reg.Register("wrap_b", func(_ map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: map[string]any{
			"path": "b.txt", "mimeType": "text/plain", "encoding": "utf-8", "content": "beta\n",
		}}, nil
	})
	return reg
}

// findOutputRecord returns the RecordKindOutput record whose aileron.step.id
// equals stepID, or fails the test.
func findOutputRecord(t *testing.T, records []AuditRecord, stepID string) AuditRecord {
	t.Helper()
	for _, r := range records {
		if r.Kind == RecordKindOutput && r.Fields["aileron.step.id"] == stepID {
			return r
		}
	}
	t.Fatalf("no output record for step %q", stepID)
	return AuditRecord{}
}

// inputEntry returns the aileron.step.inputs entry for a binding name.
func inputEntry(t *testing.T, rec AuditRecord, binding string) map[string]any {
	t.Helper()
	inputs, ok := rec.Fields["aileron.step.inputs"].([]map[string]any)
	if !ok {
		t.Fatalf("step %v: aileron.step.inputs = %v, want []map[string]any",
			rec.Fields["aileron.step.id"], rec.Fields["aileron.step.inputs"])
	}
	for _, e := range inputs {
		if e["binding"] == binding {
			return e
		}
	}
	t.Fatalf("step %v: no input entry for binding %q", rec.Fields["aileron.step.id"], binding)
	return nil
}

// TestEmitAudit_FileMapArtifactLinksUpstreamByContentHash is the load-bearing
// regression for #1912: a downstream step that binds an upstream file-map
// artifact must record its input `content_hash` in the SAME digest-space as the
// producer's `aileron.output.content_hash`, so #1891's walk-back links the two
// records instead of leaving the input dangling.
//
// It drives a real two-step chain through runPlan and asserts on the recorded
// hashes, not on hand-authored digests. It fails on origin/main (where the input
// hash is the whole-carrier digest) and passes with the fix.
func TestEmitAudit_FileMapArtifactLinksUpstreamByContentHash(t *testing.T) {
	sink := &recordingSink{}
	_, err := runPlan(context.Background(), fileMapChainPlan(), "sha256:test", "sha256:signer", nil, Options{
		Approver:   &fakeApprover{decision: Decision{Approved: true}},
		Audit:      sink,
		Clock:      FixedClock{},
		Transforms: fileMapChainRegistry(),
	})
	if err != nil {
		t.Fatalf("runPlan: %v", err)
	}

	// The producer's recorded output digest for the file-map artifact a.txt.
	producerRec := findOutputRecord(t, sink.records, "produce_a")
	producerHash, ok := producerRec.Fields["aileron.output.content_hash"].(string)
	if !ok || producerHash == "" {
		t.Fatalf("produce_a output content_hash = %v, want a non-empty string", producerRec.Fields["aileron.output.content_hash"])
	}

	consumerRec := findOutputRecord(t, sink.records, "consume_a")

	// Scenario 1 (the fix): the file-map input links to the producer by an equal
	// content_hash.
	upstream := inputEntry(t, consumerRec, "upstream")
	if upstream["source"] != "steps.produce_a.file" {
		t.Errorf("upstream source = %v, want steps.produce_a.file", upstream["source"])
	}
	if upstream["content_hash"] != producerHash {
		t.Errorf("file-map input content_hash = %v, want producer hash %v (input must link, not dangle)",
			upstream["content_hash"], producerHash)
	}

	// Scenario 2 (documents the defect the fix closes): the OLD path digested the
	// whole {path,mimeType,encoding,content} carrier, which never equals the
	// producer's content-bytes digest — the exact dangling behavior #1912 fixes.
	carrier := map[string]any{
		"path": "a.txt", "mimeType": "text/plain", "encoding": "utf-8", "content": aContent,
	}
	oldHash, err := canonicalValueDigest(carrier)
	if err != nil {
		t.Fatalf("canonicalValueDigest(carrier): %v", err)
	}
	if oldHash == producerHash {
		t.Fatal("precondition failed: the whole-carrier digest must differ from the producer's content digest for this regression to be meaningful")
	}
	if upstream["content_hash"] == oldHash {
		t.Errorf("file-map input still records the whole-carrier digest %v (dangling); the fix must digest the carried content bytes", oldHash)
	}

	// Scenario 3 (no regression): a sibling plain-data input keeps the
	// whole-value canonical digest, unchanged from prior behavior (ADR-0027 /
	// #1753). Its content_hash must equal canonicalValueDigest, not a file-map
	// content digest.
	meta := inputEntry(t, consumerRec, "meta")
	wantMeta, err := canonicalValueDigest(metaValue)
	if err != nil {
		t.Fatalf("canonicalValueDigest(metaValue): %v", err)
	}
	if meta["content_hash"] != wantMeta {
		t.Errorf("plain-data input content_hash = %v, want canonicalValueDigest %v (unchanged)", meta["content_hash"], wantMeta)
	}
}
