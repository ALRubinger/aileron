package freeze

import (
	"context"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/manifest"
)

// These manifests are built directly (not through manifest.Parse) so the
// defensive shape checks in resolveImages are exercised: resolveImages is
// the freeze-time backstop, so malformed executionEnvironment shapes that
// the schema would reject must still produce a clear error here.
func mWithExecEnv(env map[string]any) *manifest.Manifest {
	return &manifest.Manifest{
		Name: "x",
		Aileron: manifest.AileronBlock{
			Requires: manifest.Requires{ExecutionEnvironment: env},
		},
	}
}

// dummyResolver returns a resolver that yields a valid digest, so a malformed
// executionEnvironment shape surfaces its parse error rather than a
// nil-resolver error. The shape checks run before resolution, so the resolver
// is never actually called on these malformed cases.
func dummyResolver() DigestResolver {
	return DigestResolverFunc(func(context.Context, string) (string, error) { return fakeDigest, nil })
}

func TestResolveImages_BothRungsRejected(t *testing.T) {
	m := mWithExecEnv(map[string]any{
		"rung1Image":           map[string]any{"ref": "a:1"},
		"rung2CapabilityUnits": map[string]any{"features": []any{"f"}},
	})
	if _, _, err := resolveImages(context.Background(), m, nil, nil); err == nil {
		t.Error("declaring both rungs must error")
	}
}

func TestResolveImages_Rung3AlongsideRung1Rejected(t *testing.T) {
	m := mWithExecEnv(map[string]any{
		"rung1Image":         map[string]any{"ref": "a:1"},
		"rung3PerStepImages": map[string]any{"steps": []any{map[string]any{"image": "b:1"}}},
	})
	if _, _, err := resolveImages(context.Background(), m, dummyResolver(), nil); err == nil {
		t.Error("declaring rung-3 alongside rung-1 must error (exactly one image rung)")
	}
}

func TestResolveImages_Rung1MissingRef(t *testing.T) {
	m := mWithExecEnv(map[string]any{"rung1Image": map[string]any{}})
	if _, _, err := resolveImages(context.Background(), m, nil, nil); err == nil {
		t.Error("rung1Image with no ref must error")
	}
}

func TestResolveImages_Rung1NotMapping(t *testing.T) {
	m := mWithExecEnv(map[string]any{"rung1Image": "scalar"})
	if _, _, err := resolveImages(context.Background(), m, nil, nil); err == nil {
		t.Error("rung1Image that is not a mapping must error")
	}
}

func TestResolveImages_Rung2EmptyFeatures(t *testing.T) {
	m := mWithExecEnv(map[string]any{"rung2CapabilityUnits": map[string]any{"features": []any{}}})
	if _, _, err := resolveImages(context.Background(), m, nil, fakeComposer(fakeDigest)); err == nil {
		t.Error("rung2 with empty features must error")
	}
}

func TestResolveImages_Rung2EmptyFeatureEntry(t *testing.T) {
	m := mWithExecEnv(map[string]any{"rung2CapabilityUnits": map[string]any{"features": []any{""}}})
	if _, _, err := resolveImages(context.Background(), m, nil, fakeComposer(fakeDigest)); err == nil {
		t.Error("rung2 with an empty feature entry must error")
	}
}

func TestResolveImages_Rung2NotMapping(t *testing.T) {
	m := mWithExecEnv(map[string]any{"rung2CapabilityUnits": "scalar"})
	if _, _, err := resolveImages(context.Background(), m, nil, fakeComposer(fakeDigest)); err == nil {
		t.Error("rung2CapabilityUnits that is not a mapping must error")
	}
}

func TestResolveImages_Rung3NotMapping(t *testing.T) {
	m := mWithExecEnv(map[string]any{"rung3PerStepImages": "scalar"})
	if _, _, err := resolveImages(context.Background(), m, dummyResolver(), nil); err == nil {
		t.Error("rung3PerStepImages that is not a mapping must error")
	}
}

func TestResolveImages_Rung3MissingSteps(t *testing.T) {
	m := mWithExecEnv(map[string]any{"rung3PerStepImages": map[string]any{}})
	if _, _, err := resolveImages(context.Background(), m, dummyResolver(), nil); err == nil {
		t.Error("rung3PerStepImages with no steps must error")
	}
}

func TestResolveImages_Rung3EmptySteps(t *testing.T) {
	m := mWithExecEnv(map[string]any{"rung3PerStepImages": map[string]any{"steps": []any{}}})
	if _, _, err := resolveImages(context.Background(), m, dummyResolver(), nil); err == nil {
		t.Error("rung3PerStepImages with an empty steps list must error")
	}
}

func TestResolveImages_Rung3StepNotMapping(t *testing.T) {
	m := mWithExecEnv(map[string]any{"rung3PerStepImages": map[string]any{"steps": []any{"scalar"}}})
	if _, _, err := resolveImages(context.Background(), m, dummyResolver(), nil); err == nil {
		t.Error("a rung-3 step that is not a mapping must error")
	}
}

func TestResolveImages_Rung3StepEmptyImage(t *testing.T) {
	m := mWithExecEnv(map[string]any{"rung3PerStepImages": map[string]any{"steps": []any{map[string]any{"image": ""}}}})
	if _, _, err := resolveImages(context.Background(), m, dummyResolver(), nil); err == nil {
		t.Error("a rung-3 step with an empty image must error")
	}
}

func TestResolveImages_Rung3StepNonStringImage(t *testing.T) {
	m := mWithExecEnv(map[string]any{"rung3PerStepImages": map[string]any{"steps": []any{map[string]any{"image": 42}}}})
	if _, _, err := resolveImages(context.Background(), m, dummyResolver(), nil); err == nil {
		t.Error("a rung-3 step with a non-string image must error")
	}
}

func TestResolveImages_Rung3TrustContractNotMapping(t *testing.T) {
	m := mWithExecEnv(map[string]any{"rung3PerStepImages": map[string]any{"steps": []any{
		map[string]any{"image": "b:1", "trustContract": "scalar"},
	}}})
	if _, _, err := resolveImages(context.Background(), m, dummyResolver(), nil); err == nil {
		t.Error("a rung-3 step whose trustContract is not a mapping must error")
	}
}

func TestResolveImages_Rung3TrustContractNoHosts(t *testing.T) {
	m := mWithExecEnv(map[string]any{"rung3PerStepImages": map[string]any{"steps": []any{
		map[string]any{"image": "b:1", "trustContract": map[string]any{"effect": "read"}},
	}}})
	if _, _, err := resolveImages(context.Background(), m, dummyResolver(), nil); err == nil {
		t.Error("a rung-3 trustContract with no hosts must error")
	}
}

func TestResolveImages_Rung3TrustContractNonStringHost(t *testing.T) {
	m := mWithExecEnv(map[string]any{"rung3PerStepImages": map[string]any{"steps": []any{
		map[string]any{"image": "b:1", "trustContract": map[string]any{"hosts": []any{42}}},
	}}})
	if _, _, err := resolveImages(context.Background(), m, dummyResolver(), nil); err == nil {
		t.Error("a rung-3 trustContract with a non-string host must error")
	}
}

func TestResolveImages_Rung3TrustContractEmptyHosts(t *testing.T) {
	m := mWithExecEnv(map[string]any{"rung3PerStepImages": map[string]any{"steps": []any{
		map[string]any{"image": "b:1", "trustContract": map[string]any{"hosts": []any{}}},
	}}})
	if _, _, err := resolveImages(context.Background(), m, dummyResolver(), nil); err == nil {
		t.Error("a rung-3 trustContract with an empty hosts list must error")
	}
}

func TestResolveImages_NeitherRung(t *testing.T) {
	m := mWithExecEnv(map[string]any{"somethingElse": map[string]any{}})
	if _, _, err := resolveImages(context.Background(), m, nil, nil); err == nil {
		t.Error("an executionEnvironment with neither rung must error")
	}
}

func TestNilAdapters_ErrorNotPanic(t *testing.T) {
	var dr DigestResolverFunc
	if _, err := dr.ResolveDigest(context.Background(), "x"); err == nil {
		t.Error("a nil DigestResolverFunc must return an error, not panic")
	}
	var fc FeatureComposerFunc
	if _, err := fc.ComposeDigest(context.Background(), []string{"f"}); err == nil {
		t.Error("a nil FeatureComposerFunc must return an error, not panic")
	}
}

func TestVerify_MalformedPublicKey(t *testing.T) {
	priv, _ := genSigningKey(t)
	sig, _, err := Sign(priv, []byte("content"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify([]byte("content"), sig, []byte("not a pem key")); err == nil {
		t.Error("a malformed public key must fail Verify")
	}
}
