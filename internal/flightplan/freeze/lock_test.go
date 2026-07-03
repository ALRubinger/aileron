package freeze

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/manifest"
	"gopkg.in/yaml.v3"
)

func sampleLock() Lockfile {
	return Lockfile{
		ResolvedImages: []ImagePin{
			{Ref: "registry.example.com/runner:1.4", Digest: fakeDigest},
		},
		ResolvedCapabilitySet: []string{"aws-cli@2.x"},
		StepTrust: map[string]StepReach{
			"fetch": {Hosts: []string{"s3.amazonaws.com"}},
			"file":  {Hosts: []string{"tracker.example.com", "tracker.example.com:443"}},
		},
		ContentHash: fakeDigest2,
		Version:     "1.2.3",
	}
}

func TestMarshalLockfile_RoundTrip(t *testing.T) {
	want := sampleLock()
	b, err := MarshalLockfile(want)
	if err != nil {
		t.Fatalf("MarshalLockfile: %v", err)
	}
	var got Lockfile
	if err := yaml.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

// The marshaled lock must match the schema `$defs.lock` field names so the
// injected block validates and the standalone artifact is the same shape.
func TestMarshalLockfile_UsesSchemaFieldNames(t *testing.T) {
	b, err := MarshalLockfile(sampleLock())
	if err != nil {
		t.Fatalf("MarshalLockfile: %v", err)
	}
	s := string(b)
	for _, key := range []string{"resolvedImages:", "resolvedCapabilitySet:", "stepTrust:", "contentHash:", "version:", "ref:", "digest:", "hosts:"} {
		if !strings.Contains(s, key) {
			t.Errorf("lockfile missing schema field %q:\n%s", key, s)
		}
	}
}

// TestMarshalLockfile_StepTrustDeterministic proves the step-keyed map
// marshals byte-identically regardless of insertion order: yaml.v3 sorts map
// keys, and the freeze determinism contract (two freezes of the same input
// produce byte-identical lockfiles) rides on it.
func TestMarshalLockfile_StepTrustDeterministic(t *testing.T) {
	ab := Lockfile{StepTrust: map[string]StepReach{}}
	ab.StepTrust["alpha"] = StepReach{Hosts: []string{"a.example.com"}}
	ab.StepTrust["beta"] = StepReach{Hosts: []string{"b.example.com"}}
	ba := Lockfile{StepTrust: map[string]StepReach{}}
	ba.StepTrust["beta"] = StepReach{Hosts: []string{"b.example.com"}}
	ba.StepTrust["alpha"] = StepReach{Hosts: []string{"a.example.com"}}

	first, err := MarshalLockfile(ab)
	if err != nil {
		t.Fatalf("marshal ab: %v", err)
	}
	second, err := MarshalLockfile(ba)
	if err != nil {
		t.Fatalf("marshal ba: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("stepTrust marshal is insertion-order dependent:\n%s\nvs\n%s", first, second)
	}
	idxAlpha := strings.Index(string(first), "alpha:")
	idxBeta := strings.Index(string(first), "beta:")
	if idxAlpha < 0 || idxBeta < 0 || idxAlpha > idxBeta {
		t.Errorf("stepTrust keys must emit sorted:\n%s", first)
	}
}

func TestInjectLock_PreservesSiblingKeysAndReparses(t *testing.T) {
	raw := exampleSkillMD(t)
	out, err := injectLock(raw, sampleLock())
	if err != nil {
		t.Fatalf("injectLock: %v", err)
	}

	// Re-parses cleanly through manifest.Parse with the lock present.
	m, err := manifest.Parse(out)
	if err != nil {
		t.Fatalf("re-parse frozen manifest: %v", err)
	}
	if m.Name != "weekly-metrics-digest" {
		t.Errorf("name lost on inject: %q", m.Name)
	}
	if m.Aileron.SchemaVersion != "aileron.flightplan.v1" {
		t.Errorf("schemaVersion lost: %q", m.Aileron.SchemaVersion)
	}
	if len(m.Aileron.Requires.Actions) != 2 {
		t.Errorf("requires.actions lost: %d", len(m.Aileron.Requires.Actions))
	}
	if len(m.Aileron.Steps) != 4 {
		t.Errorf("steps lost: %d", len(m.Aileron.Steps))
	}
	if m.Aileron.Lock["contentHash"] != fakeDigest2 {
		t.Errorf("lock.contentHash = %v", m.Aileron.Lock["contentHash"])
	}

	// Body preserved.
	if !strings.Contains(m.Body, "# Weekly Metrics Digest") {
		t.Error("markdown body not preserved through injection")
	}
}

func TestInjectLock_ReplacesExistingLock(t *testing.T) {
	raw := exampleSkillMD(t)
	first, err := injectLock(raw, Lockfile{Version: "1.0.0", ContentHash: fakeDigest})
	if err != nil {
		t.Fatalf("first inject: %v", err)
	}
	second, err := injectLock(first, Lockfile{Version: "2.0.0", ContentHash: fakeDigest2})
	if err != nil {
		t.Fatalf("second inject: %v", err)
	}
	m, err := manifest.Parse(second)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Aileron.Lock["version"] != "2.0.0" {
		t.Errorf("lock not replaced: version = %v", m.Aileron.Lock["version"])
	}
	// Only one lock key (no duplicate).
	if strings.Count(string(second), "\n  lock:") > 1 {
		t.Errorf("duplicate lock block:\n%s", second)
	}
}

func TestInjectLock_NoAileronBlock(t *testing.T) {
	if _, err := injectLock([]byte(instructionOnlyMD), sampleLock()); err == nil {
		t.Error("injectLock into an instruction-only manifest must error")
	}
}

func TestInjectLock_NoFrontmatter(t *testing.T) {
	if _, err := injectLock([]byte("no frontmatter here"), sampleLock()); err == nil {
		t.Error("injectLock without frontmatter must error")
	}
}

func TestInjectLock_AileronNotAMapping(t *testing.T) {
	// An `aileron` key whose value is a scalar (not a mapping) is a
	// malformed manifest; injectLock must error rather than corrupt it.
	const bad = "---\nname: x\naileron: not-a-mapping\n---\nbody\n"
	if _, err := injectLock([]byte(bad), sampleLock()); err == nil {
		t.Error("injectLock must error when aileron is not a mapping")
	}
}

func TestInjectLock_FrontmatterNotAMapping(t *testing.T) {
	// Frontmatter that is a YAML sequence, not a mapping.
	const bad = "---\n- a\n- b\n---\nbody\n"
	if _, err := injectLock([]byte(bad), sampleLock()); err == nil {
		t.Error("injectLock must error when frontmatter is not a mapping")
	}
}

func TestInjectLock_CRLFProducesStableLFOutput(t *testing.T) {
	lf := exampleSkillMD(t)
	crlf := []byte(strings.ReplaceAll(string(lf), "\n", "\r\n"))

	outLF, err := injectLock(lf, sampleLock())
	if err != nil {
		t.Fatalf("inject LF: %v", err)
	}
	outCRLF, err := injectLock(crlf, sampleLock())
	if err != nil {
		t.Fatalf("inject CRLF: %v", err)
	}
	if string(outLF) != string(outCRLF) {
		t.Error("a CRLF SKILL.md must freeze to byte-identical output as its LF twin")
	}
	// And the output is LF-canonical (no stray CR).
	if strings.Contains(string(outCRLF), "\r") {
		t.Error("frozen output must be LF-canonical")
	}
}

func TestInjectLockMaybe_InstructionOnlyReturnsCanonicalRaw(t *testing.T) {
	crlf := []byte(strings.ReplaceAll(instructionOnlyMD, "\n", "\r\n"))
	out, err := injectLockMaybe(crlf, sampleLock(), true)
	if err != nil {
		t.Fatalf("injectLockMaybe: %v", err)
	}
	if strings.Contains(string(out), "lock:") {
		t.Error("an instruction-only manifest must not gain a lock block")
	}
	if strings.Contains(string(out), "\r") {
		t.Error("instruction-only frozen output must be LF-canonical")
	}
}

func TestWithoutContentHash_ClearsHashOnly(t *testing.T) {
	l := sampleLock()
	got := l.withoutContentHash()
	if got.ContentHash != "" {
		t.Errorf("ContentHash not cleared: %q", got.ContentHash)
	}
	if got.Version != "1.2.3" || len(got.ResolvedImages) != 1 {
		t.Error("withoutContentHash must only clear the hash")
	}
}
