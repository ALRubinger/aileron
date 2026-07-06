package credreq

import (
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/flightplan/manifest"
	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
)

// --- Unit B: slug + credential_ref derivation ---

// assertValidUserRef proves a derived credential_ref is accepted by the real
// credential-ref contract (internal/binding), so the derivation can never
// produce a ref the binding layer would reject at construction.
func assertValidUserRef(t *testing.T, ref string) {
	t.Helper()
	if _, err := binding.NewHostBinding("api.example.com", ref, binding.SchemeBearer); err != nil {
		t.Fatalf("derived credential_ref %q is not a valid credential ref: %v", ref, err)
	}
}

// TestDeriveCredentialRef_LabelPresent proves a contract with an identity label
// derives user/<slug(label)>.
func TestDeriveCredentialRef_LabelPresent(t *testing.T) {
	got := deriveCredentialRef("aws-sigv4", "prod-reader")
	if got != "user/prod-reader" {
		t.Errorf("ref = %q, want user/prod-reader", got)
	}
	assertValidUserRef(t, got)
}

// TestDeriveCredentialRef_LabelAbsentFallsBackToKind proves a contract with no
// identity label falls back to user/<slug(kind)>.
func TestDeriveCredentialRef_LabelAbsentFallsBackToKind(t *testing.T) {
	got := deriveCredentialRef("aws-sigv4", "")
	if got != "user/aws-sigv4" {
		t.Errorf("ref = %q, want user/aws-sigv4", got)
	}
	assertValidUserRef(t, got)
}

// TestDeriveCredentialRef_MessyLabelSlugsToValidRef proves a messy label
// (spaces, dots, mixed case, unicode, leading/trailing punctuation) slugs to a
// ref that is valid under the user-ref grammar (no dots, starts alnum).
func TestDeriveCredentialRef_MessyLabelSlugsToValidRef(t *testing.T) {
	cases := []struct {
		label string
		want  string
	}{
		{"Prod Reader", "user/prod-reader"},
		{"acct.us-east-1.prod", "user/acct-us-east-1-prod"},
		{"  Léon's Key!  ", "user/l-on-s-key"},
		{"UPPER_snake", "user/upper-snake"},
		{"...dots...", "user/dots"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			got := deriveCredentialRef("aws-sigv4", tc.label)
			if got != tc.want {
				t.Errorf("ref = %q, want %q", got, tc.want)
			}
			assertValidUserRef(t, got)
		})
	}
}

// TestDeriveCredentialRef_EmptySluggingLabelFallsThrough proves a label that
// slugs to empty (no alnum) falls back to the kind slug, and a kind that also
// slugs to empty falls back to the stable token, so the ref is always
// well-formed.
func TestDeriveCredentialRef_EmptySluggingLabelFallsThrough(t *testing.T) {
	// Label "***" slugs to empty; kind "aws-sigv4" slugs to a valid segment.
	got := deriveCredentialRef("aws-sigv4", "***")
	if got != "user/aws-sigv4" {
		t.Errorf("ref = %q, want user/aws-sigv4 (label slugs empty, fall back to kind)", got)
	}
	assertValidUserRef(t, got)

	// Both label and kind slug to empty: the stable fallback keeps the ref valid.
	got = deriveCredentialRef("***", "")
	if got != "user/"+unnamedCredentialService {
		t.Errorf("ref = %q, want user/%s (both slug empty)", got, unnamedCredentialService)
	}
	assertValidUserRef(t, got)
}

// TestDeriveCredentialRef_CollisionSurface documents that two distinct labels
// that slug identically collapse to the same ref. This is the exact, stable
// rule; the test pins the collision surface so a change to it is visible.
func TestDeriveCredentialRef_CollisionSurface(t *testing.T) {
	a := deriveCredentialRef("aws-sigv4", "Prod Reader")
	b := deriveCredentialRef("aws-sigv4", "prod.reader")
	if a != b {
		t.Errorf("expected %q and %q to slug to the same ref (documented collision), got %q vs %q", "Prod Reader", "prod.reader", a, b)
	}
}

// --- fixture helpers: SKILL.md -> manifest.Parse -> runtime.Decode ---

// decodePlan renders an inline SKILL.md carrying the given inputs and steps
// blocks, runs it through the real parse + decode path, and returns the decoded
// plan. Exercising manifest.Parse + runtime.Decode (not a hand-built *Plan)
// keeps the fixtures honest: the TrustContract values Derive sees are exactly
// what the shared DTO validator produces.
func decodePlan(t *testing.T, inputsItems, stepsItems string) *runtime.Plan {
	t.Helper()
	inputsBlock := "  inputs: []\n"
	if inputsItems != "" {
		inputsBlock = "  inputs:\n" + inputsItems
	}
	md := "---\n" +
		"name: credreq-fixture\n" +
		"description: credreq derivation fixture.\n" +
		"aileron:\n" +
		"  schemaVersion: aileron.flightplan.v1\n" +
		"  environment:\n" +
		"    tools: [aws-cli@2.x]\n" +
		inputsBlock +
		"  outputs: []\n" +
		"  steps:\n" +
		stepsItems +
		"---\n# fixture\n"
	m, err := manifest.Parse([]byte(md))
	if err != nil {
		t.Fatalf("manifest.Parse: %v\n---\n%s", err, md)
	}
	p, err := runtime.Decode(m)
	if err != nil {
		t.Fatalf("runtime.Decode: %v", err)
	}
	return p
}

// sigv4Step renders an aws-sigv4 tool step. An empty label omits identityLabel.
func sigv4Step(id, label, host string) string {
	cred := "{ kind: aws-sigv4, placement: signing"
	if label != "" {
		cred += ", identityLabel: " + label
	}
	cred += " }"
	return "    - id: " + id + "\n" +
		"      kind: tool\n" +
		"      command: [tool]\n" +
		"      outputs: [" + id + "_out]\n" +
		"      trustContract:\n" +
		"        credential: " + cred + "\n" +
		"        hosts: [\"" + host + "\"]\n" +
		"        effect: read\n" +
		"        idempotency: { safeToRetry: true }\n" +
		"        audit: { fields: [result] }\n"
}

// oauth2Step renders an oauth2 (host-keyed) tool step.
func oauth2Step(id, label, host string) string {
	return "    - id: " + id + "\n" +
		"      kind: tool\n" +
		"      command: [tool]\n" +
		"      outputs: [" + id + "_out]\n" +
		"      trustContract:\n" +
		"        credential: { kind: oauth2, placement: header, identityLabel: " + label + " }\n" +
		"        oauth: { scopes: [read] }\n" +
		"        hosts: [\"" + host + "\"]\n" +
		"        effect: read\n" +
		"        idempotency: { safeToRetry: true }\n" +
		"        audit: { fields: [result] }\n"
}

// noneStep renders an unauthenticated (kind: none) tool step.
func noneStep(id, host string) string {
	return "    - id: " + id + "\n" +
		"      kind: tool\n" +
		"      command: [tool]\n" +
		"      outputs: [" + id + "_out]\n" +
		"      trustContract:\n" +
		"        credential: { kind: none }\n" +
		"        hosts: [\"" + host + "\"]\n" +
		"        effect: read\n" +
		"        idempotency: { safeToRetry: true }\n" +
		"        audit: { fields: [result] }\n"
}

// plainToolStep renders a tool step with no trust contract.
func plainToolStep(id string) string {
	return "    - id: " + id + "\n" +
		"      kind: tool\n" +
		"      command: [tool]\n" +
		"      outputs: [" + id + "_out]\n"
}

// awsRegionInput declares a constrained aws_region input for templated-host
// fixtures.
const awsRegionInput = "    - name: aws_region\n" +
	"      type: string\n" +
	"      resolution: { rule: literal, default: us-east-1 }\n" +
	"      constraint: { enum: [us-east-1, us-west-2] }\n"

// --- Unit C: Derive core ---

// TestDerive_SingleSigv4Step proves one aws-sigv4 tool step yields exactly one
// binding: sigv4-resign scheme, host-less-identity, credential_ref
// user/<slug(identityLabel)>, hosts carried.
func TestDerive_SingleSigv4Step(t *testing.T) {
	p := decodePlan(t, "", sigv4Step("q1", "prod-reader", "athena.us-east-1.amazonaws.com"))
	got, err := Derive(p)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d bindings, want 1: %+v", len(got), got)
	}
	rb := got[0]
	if rb.CredentialKind != "aws-sigv4" {
		t.Errorf("kind = %q, want aws-sigv4", rb.CredentialKind)
	}
	if rb.Scheme != "sigv4-resign" {
		t.Errorf("scheme = %q, want sigv4-resign", rb.Scheme)
	}
	if !rb.HostLessIdentity() {
		t.Errorf("shape = %q, want host-less-identity", rb.HostShape)
	}
	if rb.CredentialRef != "user/prod-reader" {
		t.Errorf("ref = %q, want user/prod-reader", rb.CredentialRef)
	}
	if strings.Join(rb.Hosts, ",") != "athena.us-east-1.amazonaws.com" {
		t.Errorf("hosts = %v, want the declared host", rb.Hosts)
	}
	if len(rb.TemplatedHosts) != 0 {
		t.Errorf("templated hosts = %v, want none", rb.TemplatedHosts)
	}
	assertValidUserRef(t, rb.CredentialRef)
}

// TestDerive_TwoSigv4StepsSharedIdentityDeduplicate proves two aws-sigv4 steps
// that share one identity label collapse to a single binding even when their
// (templated) hosts differ; the merged binding's Hosts is the union.
func TestDerive_TwoSigv4StepsSharedIdentityDeduplicate(t *testing.T) {
	steps := sigv4Step("q1", "prod-reader", "athena.us-east-1.amazonaws.com") +
		sigv4Step("q2", "prod-reader", "athena.{{ inputs.aws_region }}.amazonaws.com")
	p := decodePlan(t, awsRegionInput, steps)
	got, err := Derive(p)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d bindings, want 1 (shared identity dedup): %+v", len(got), got)
	}
	rb := got[0]
	// Union of both steps' hosts, sorted.
	wantHosts := "athena.us-east-1.amazonaws.com,athena.{{ inputs.aws_region }}.amazonaws.com"
	if strings.Join(rb.Hosts, ",") != wantHosts {
		t.Errorf("hosts = %v, want the union %q", rb.Hosts, wantHosts)
	}
	// The templated host is flagged (present but harmless for sigv4).
	if strings.Join(rb.TemplatedHosts, ",") != "athena.{{ inputs.aws_region }}.amazonaws.com" {
		t.Errorf("templated hosts = %v, want the one templated host", rb.TemplatedHosts)
	}
}

// TestDerive_OAuth2HostKeyedStep proves an oauth2 step yields one host-keyed
// binding with the bearer scheme and the declared hosts populated.
func TestDerive_OAuth2HostKeyedStep(t *testing.T) {
	p := decodePlan(t, "", oauth2Step("s1", "workspace-a", "api.example.com"))
	got, err := Derive(p)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d bindings, want 1: %+v", len(got), got)
	}
	rb := got[0]
	if rb.Scheme != "bearer" {
		t.Errorf("scheme = %q, want bearer", rb.Scheme)
	}
	if rb.HostLessIdentity() {
		t.Errorf("shape = %q, want host-keyed", rb.HostShape)
	}
	if strings.Join(rb.Hosts, ",") != "api.example.com" {
		t.Errorf("hosts = %v, want the declared host", rb.Hosts)
	}
	if rb.CredentialRef != "user/workspace-a" {
		t.Errorf("ref = %q, want user/workspace-a", rb.CredentialRef)
	}
}

// TestDerive_HostKeyedDistinctHostSetsStayDistinct proves two host-keyed steps
// with the same identity but different host sets remain two bindings (hosts are
// part of the host-keyed dedup key).
func TestDerive_HostKeyedDistinctHostSetsStayDistinct(t *testing.T) {
	steps := oauth2Step("s1", "workspace-a", "api.one.example.com") +
		oauth2Step("s2", "workspace-a", "api.two.example.com")
	p := decodePlan(t, "", steps)
	got, err := Derive(p)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d bindings, want 2 (distinct host sets): %+v", len(got), got)
	}
	// Deterministic order: sorted by hosts (same ref/kind/label/scheme).
	if strings.Join(got[0].Hosts, ",") != "api.one.example.com" || strings.Join(got[1].Hosts, ",") != "api.two.example.com" {
		t.Errorf("bindings not in deterministic host order: %v / %v", got[0].Hosts, got[1].Hosts)
	}
}

// TestDerive_TemplatedHostFlaggedHostKeyed proves a host-keyed step's templated
// host is represented in Hosts and listed in TemplatedHosts.
func TestDerive_TemplatedHostFlaggedHostKeyed(t *testing.T) {
	p := decodePlan(t, awsRegionInput, oauth2Step("s1", "workspace-a", "athena.{{ inputs.aws_region }}.amazonaws.com"))
	got, err := Derive(p)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d bindings, want 1: %+v", len(got), got)
	}
	rb := got[0]
	host := "athena.{{ inputs.aws_region }}.amazonaws.com"
	if strings.Join(rb.Hosts, ",") != host {
		t.Errorf("hosts = %v, want %q", rb.Hosts, host)
	}
	if strings.Join(rb.TemplatedHosts, ",") != host {
		t.Errorf("templated hosts = %v, want %q flagged", rb.TemplatedHosts, host)
	}
}

// TestDerive_NoneStepProducesNoBinding proves a kind: none tool step onboards
// no credential and yields no requirement.
func TestDerive_NoneStepProducesNoBinding(t *testing.T) {
	p := decodePlan(t, "", noneStep("s1", "api.example.com"))
	got, err := Derive(p)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d bindings, want 0 for a none-kind step: %+v", len(got), got)
	}
}

// TestDerive_IgnoresNonToolAndUncontractedSteps proves non-tool steps and tool
// steps with no trust contract are ignored, leaving only the contracted tool
// step's binding.
func TestDerive_IgnoresNonToolAndUncontractedSteps(t *testing.T) {
	steps := plainToolStep("plain") +
		"    - id: shape\n" +
		"      kind: transform\n" +
		"      bindings: { in: steps.plain.plain_out }\n" +
		"      outputs: [shaped]\n" +
		sigv4Step("q1", "prod-reader", "athena.us-east-1.amazonaws.com")
	p := decodePlan(t, "", steps)
	got, err := Derive(p)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d bindings, want 1 (only the contracted tool step): %+v", len(got), got)
	}
	if got[0].CredentialKind != "aws-sigv4" {
		t.Errorf("binding kind = %q, want the sigv4 step's", got[0].CredentialKind)
	}
}

// TestDerive_MixedKindsSortedDeterministically proves a plan mixing an oauth2
// and an aws-sigv4 step yields both bindings in the documented, stable order
// (sorted by credential_ref first).
func TestDerive_MixedKindsSortedDeterministically(t *testing.T) {
	steps := oauth2Step("s1", "workspace-a", "api.example.com") +
		sigv4Step("q1", "prod-reader", "athena.us-east-1.amazonaws.com")
	p := decodePlan(t, "", steps)
	got, err := Derive(p)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d bindings, want 2: %+v", len(got), got)
	}
	// user/prod-reader < user/workspace-a lexically.
	if got[0].CredentialRef != "user/prod-reader" || got[1].CredentialRef != "user/workspace-a" {
		t.Errorf("order = [%q, %q], want [user/prod-reader, user/workspace-a]", got[0].CredentialRef, got[1].CredentialRef)
	}
}

// TestDerive_UnmappedKindFailsClosed proves a tool-step contract whose
// credential.kind is outside the closed scheme table makes Derive return an
// error (no guessed scheme). The schema enum blocks such a kind from a decoded
// fixture, so the plan is constructed directly over the exported runtime types.
func TestDerive_UnmappedKindFailsClosed(t *testing.T) {
	p := &runtime.Plan{
		Steps: []runtime.Step{{
			ID:      "s1",
			Kind:    runtime.KindTool,
			Command: []string{"tool"},
			TrustContract: &runtime.TrustContract{
				CredentialKind: "exotic-kind",
				Hosts:          []string{"api.example.com"},
				Effect:         runtime.EffectRead,
			},
		}},
	}
	_, err := Derive(p)
	if err == nil {
		t.Fatal("an unmapped credential kind must fail closed")
	}
	if !strings.Contains(err.Error(), "exotic-kind") {
		t.Errorf("error %q should name the offending kind", err)
	}
}

// apiKeyStep renders an api-key (host-keyed) tool step.
func apiKeyStep(id, label, host string) string {
	return "    - id: " + id + "\n" +
		"      kind: tool\n" +
		"      command: [tool]\n" +
		"      outputs: [" + id + "_out]\n" +
		"      trustContract:\n" +
		"        credential: { kind: api-key, placement: header, identityLabel: " + label + " }\n" +
		"        hosts: [\"" + host + "\"]\n" +
		"        effect: read\n" +
		"        idempotency: { safeToRetry: true }\n" +
		"        audit: { fields: [result] }\n"
}

// TestDerive_ApiKeyMapsToBearerHostKeyed proves the documented api-key default:
// it maps to the bearer scheme and a host-keyed shape (the header-bearer wire
// form). It also pins the same-credential_ref ordering tie-break: an api-key
// and an oauth2 step sharing one label derive the same ref (user/<label>) and
// order by credential kind, so api-key precedes oauth2 deterministically.
func TestDerive_ApiKeyMapsToBearerHostKeyed(t *testing.T) {
	steps := oauth2Step("s2", "shared", "api.example.com") +
		apiKeyStep("s1", "shared", "api.example.com")
	p := decodePlan(t, "", steps)
	got, err := Derive(p)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d bindings, want 2: %+v", len(got), got)
	}
	// Same host set + same label -> same ref, so the (kind) tie-break orders
	// them: api-key < oauth2.
	if got[0].CredentialRef != "user/shared" || got[1].CredentialRef != "user/shared" {
		t.Fatalf("both refs should be user/shared, got %q and %q", got[0].CredentialRef, got[1].CredentialRef)
	}
	if got[0].CredentialKind != "api-key" || got[1].CredentialKind != "oauth2" {
		t.Errorf("kind order = [%q, %q], want [api-key, oauth2]", got[0].CredentialKind, got[1].CredentialKind)
	}
	if got[0].Scheme != "bearer" || got[0].HostLessIdentity() {
		t.Errorf("api-key binding = scheme %q shape %q, want bearer/host-keyed", got[0].Scheme, got[0].HostShape)
	}
}

// TestDerive_MalformedTemplatedHostFailsClosed proves a malformed
// `{{ inputs...` token in a mapped step's host makes Derive fail closed. The
// decode path validates the grammar, so this defensive path is reached via a
// directly-constructed plan; it guarantees Derive never emits a binding for a
// host it could not scan.
func TestDerive_MalformedTemplatedHostFailsClosed(t *testing.T) {
	p := &runtime.Plan{
		Steps: []runtime.Step{{
			ID:      "s1",
			Kind:    runtime.KindTool,
			Command: []string{"tool"},
			TrustContract: &runtime.TrustContract{
				CredentialKind: "aws-sigv4",
				IdentityLabel:  "prod-reader",
				Hosts:          []string{"athena.{{ inputs.region"},
				Effect:         runtime.EffectRead,
			},
		}},
	}
	if _, err := Derive(p); err == nil {
		t.Fatal("a malformed host template must fail closed")
	}
}

// TestDerive_DuplicateHostsInOneContractCollapse proves repeated hosts within a
// single contract collapse to one entry in the derived binding's Hosts.
func TestDerive_DuplicateHostsInOneContractCollapse(t *testing.T) {
	p := &runtime.Plan{
		Steps: []runtime.Step{{
			ID:      "s1",
			Kind:    runtime.KindTool,
			Command: []string{"tool"},
			TrustContract: &runtime.TrustContract{
				CredentialKind: "oauth2",
				IdentityLabel:  "workspace-a",
				Hosts:          []string{"api.example.com", "api.example.com"},
				Effect:         runtime.EffectRead,
			},
		}},
	}
	got, err := Derive(p)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if len(got) != 1 || strings.Join(got[0].Hosts, ",") != "api.example.com" {
		t.Fatalf("duplicate hosts must collapse to one, got %+v", got)
	}
}

// TestDerive_CaseVariantHostsCollapse proves two host-keyed steps whose hosts
// differ only in case are one logical requirement: they merge to a single
// binding whose Hosts carries one entry (a verbatim casing), never two
// case-variant duplicates.
func TestDerive_CaseVariantHostsCollapse(t *testing.T) {
	p := &runtime.Plan{
		Steps: []runtime.Step{
			{
				ID: "s1", Kind: runtime.KindTool, Command: []string{"tool"},
				TrustContract: &runtime.TrustContract{
					CredentialKind: "oauth2", IdentityLabel: "workspace-a",
					Hosts: []string{"API.Example.com"}, Effect: runtime.EffectRead,
				},
			},
			{
				ID: "s2", Kind: runtime.KindTool, Command: []string{"tool"},
				TrustContract: &runtime.TrustContract{
					CredentialKind: "oauth2", IdentityLabel: "workspace-a",
					Hosts: []string{"api.example.com"}, Effect: runtime.EffectRead,
				},
			},
		},
	}
	got, err := Derive(p)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("case-variant hosts must collapse to one binding, got %d: %+v", len(got), got)
	}
	if len(got[0].Hosts) != 1 {
		t.Errorf("Hosts = %v, want a single case-insensitively-deduplicated entry", got[0].Hosts)
	}
	// The retained entry is a verbatim casing (the lexicographically smallest),
	// never a synthesized lowercase form.
	if got[0].Hosts[0] != "API.Example.com" {
		t.Errorf("Hosts[0] = %q, want the verbatim smallest casing API.Example.com", got[0].Hosts[0])
	}
}

// TestDerive_NilPlan proves Derive rejects a nil plan rather than panicking.
func TestDerive_NilPlan(t *testing.T) {
	if _, err := Derive(nil); err == nil {
		t.Fatal("Derive(nil) must return an error")
	}
}

// TestDerive_EmptyPlanYieldsNoBindings proves a plan with no contracted tool
// steps derives an empty (non-error) set.
func TestDerive_EmptyPlanYieldsNoBindings(t *testing.T) {
	p := decodePlan(t, "", plainToolStep("plain"))
	got, err := Derive(p)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d bindings, want 0: %+v", len(got), got)
	}
}
