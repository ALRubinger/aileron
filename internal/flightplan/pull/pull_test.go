package pull

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"

	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

// noEnvironmentMD is a valid manifest with an aileron block but no environment
// block, so freeze.Run seals a real (verifiable) lock without needing an image
// resolver. Its name keys the store directory the pulled version lands under.
const noEnvironmentMD = `---
name: rubber-duck
description: A skill with an aileron block but no environment.
aileron:
  schemaVersion: aileron.flightplan.v1
  requires:
    actions:
      - ref: aileron:metrics.query_series
        trustContract:
          credential:
            kind: none
          hosts:
            - api.example.com
          effect: read
          idempotency:
            safeToRetry: true
          audit:
            fields:
              - result
  inputs: []
  outputs: []
---

# Rubber Duck
Explain it out loud.
`

// genSigningKeyPath writes a fresh ed25519 PKCS#8 PEM private key to a temp
// file and returns its path, so freeze.Run can sign a real artifact.
func genSigningKeyPath(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	path := filepath.Join(t.TempDir(), "signing-key.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return path
}

// mintArtifact freezes instructionOnlyMD (optionally attributing it to a
// publisher) and returns the real signed artifact bytes plus the freeze slug
// used as the version tag.
func mintArtifact(t *testing.T, publisher string) (freeze.Result, string) {
	t.Helper()
	res, err := freeze.Run(context.Background(), []byte(noEnvironmentMD), freeze.Options{
		Publisher:      publisher,
		SigningKeyPath: genSigningKeyPath(t),
	})
	if err != nil {
		t.Fatalf("freeze.Run: %v", err)
	}
	// The version tag is the content-hash slug (algo dropped, hex used as the
	// single-path-segment id). Use a stable slug the store will accept.
	return res, "v" + res.ContentHash[len("sha256:"):][:12]
}

// seedArtifact pushes the four signed-artifact layers and a referrers manifest
// tagged at tag into st, mirroring how publish writes the artifact. It records
// tag as the manifest's content-hash version annotation, so a straightforward
// content-hash-tag install resolves the same id via the annotation. layers lets
// a test omit or corrupt a specific media type; artifactType lets a test push a
// wrong type. It returns the manifest descriptor.
func seedArtifact(t *testing.T, st *memory.Store, tag, artifactType string, layers map[string][]byte) ocispec.Descriptor {
	t.Helper()
	return seedArtifactVersioned(t, st, tag, tag, artifactType, layers)
}

// seedArtifactVersioned is seedArtifact with an explicit content-hash version
// annotation distinct from the tag, so a test can seed a MOVING tag (latest, a
// semver) whose manifest still carries the immutable content-hash slug. An empty
// version omits the annotation, exercising the literal-tag fallback.
func seedArtifactVersioned(t *testing.T, st *memory.Store, tag, version, artifactType string, layers map[string][]byte) ocispec.Descriptor {
	t.Helper()
	ctx := context.Background()
	var descs []ocispec.Descriptor
	for mt, data := range layers {
		d := content.NewDescriptorFromBytes(mt, data)
		if err := st.Push(ctx, d, bytes.NewReader(data)); err != nil {
			t.Fatalf("push layer %s: %v", mt, err)
		}
		descs = append(descs, d)
	}
	var annotations map[string]string
	if version != "" {
		annotations = map[string]string{freeze.AnnotationVersion: version}
	}
	m := ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: artifactType,
		Config:       ocispec.DescriptorEmptyJSON,
		Layers:       descs,
		Annotations:  annotations,
	}
	// The empty config blob must be present for FetchAll traversal parity.
	if err := st.Push(ctx, ocispec.DescriptorEmptyJSON, bytes.NewReader(ocispec.DescriptorEmptyJSON.Data)); err != nil {
		t.Fatalf("push empty config: %v", err)
	}
	mb, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	md := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, mb)
	if err := st.Push(ctx, md, bytes.NewReader(mb)); err != nil {
		t.Fatalf("push manifest: %v", err)
	}
	if err := st.Tag(ctx, md, tag); err != nil {
		t.Fatalf("tag manifest: %v", err)
	}
	return md
}

func allLayers(res freeze.Result) map[string][]byte {
	return map[string][]byte{
		freeze.MediaTypeSkillMD:   res.FrozenManifest,
		freeze.MediaTypeLock:      res.Lockfile,
		freeze.MediaTypeSignature: res.Signature,
		freeze.MediaTypePublicKey: res.PublicKey,
	}
}

// recordingVerifier records the arguments it was called with and returns err.
type recordingVerifier struct {
	err       error
	publisher string
	key       ed25519.PublicKey
	called    bool
}

func (r *recordingVerifier) VerifyPublisher(publisher string, key ed25519.PublicKey) error {
	r.called = true
	r.publisher = publisher
	r.key = key
	return r.err
}

func TestRunHappyPath(t *testing.T) {
	res, tag := mintArtifact(t, "")
	st := memory.New()
	seedArtifact(t, st, tag, freeze.ArtifactType, allLayers(res))

	got, err := Run(context.Background(), Options{Ref: "localhost:5000/demo:" + tag, Source: st})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Name != "rubber-duck" {
		t.Errorf("name = %q, want rubber-duck", got.Name)
	}
	if got.Frozen.ID != tag {
		t.Errorf("frozen id = %q, want tag %q", got.Frozen.ID, tag)
	}
	if !bytes.Equal(got.Frozen.SkillMD, res.FrozenManifest) {
		t.Error("SkillMD bytes differ from published artifact")
	}
	if !bytes.Equal(got.Frozen.Signature, res.Signature) {
		t.Error("Signature bytes differ from published artifact")
	}
	if !bytes.Equal(got.Frozen.PublicKey, res.PublicKey) {
		t.Error("PublicKey bytes differ from published artifact")
	}
	// The Result carries the parsed source coordinate so the CLI can persist the
	// install origin for launch (#1903).
	if got.SourceRegistry != "localhost:5000/demo" {
		t.Errorf("source registry = %q, want localhost:5000/demo", got.SourceRegistry)
	}
	if got.SourceTag != tag {
		t.Errorf("source tag = %q, want %q", got.SourceTag, tag)
	}
}

func TestRunMissingReferrerIsMissingArtifact(t *testing.T) {
	st := memory.New() // nothing seeded
	_, err := Run(context.Background(), Options{Ref: "localhost:5000/demo:v1abc", Source: st})
	if !errors.Is(err, ErrMissingArtifact) {
		t.Fatalf("err = %v, want ErrMissingArtifact", err)
	}
}

// resolveFailTarget wraps a memory store but fails every Resolve with a
// non-not-found error, standing in for an unreachable / unauthenticated
// registry (where the failure is NOT a missing artifact).
type resolveFailTarget struct {
	*memory.Store
	err error
}

func (r resolveFailTarget) Resolve(context.Context, string) (ocispec.Descriptor, error) {
	return ocispec.Descriptor{}, r.err
}

func TestRunUnreachableRegistryNotMissingArtifact(t *testing.T) {
	unreachable := errors.New("dial tcp: connection refused")
	src := resolveFailTarget{Store: memory.New(), err: unreachable}
	_, err := Run(context.Background(), Options{Ref: "localhost:5000/demo:v1abc", Source: src})
	if err == nil {
		t.Fatal("want an error for an unreachable registry")
	}
	if errors.Is(err, ErrMissingArtifact) {
		t.Errorf("err = %v, must NOT be classified as ErrMissingArtifact", err)
	}
	if !errors.Is(err, unreachable) {
		t.Errorf("err = %v, want the underlying resolve error to remain inspectable", err)
	}
}

// fetchFailTarget resolves like its embedded store but fails every Fetch,
// standing in for a registry that resolves a descriptor yet cannot serve its
// bytes (transient / truncated read).
type fetchFailTarget struct {
	*memory.Store
	err error
}

func (f fetchFailTarget) Fetch(context.Context, ocispec.Descriptor) (io.ReadCloser, error) {
	return nil, f.err
}

func TestRunManifestFetchErrorSurfaces(t *testing.T) {
	res, tag := mintArtifact(t, "")
	inner := memory.New()
	seedArtifact(t, inner, tag, freeze.ArtifactType, allLayers(res))
	fetchErr := errors.New("registry read reset")
	_, err := Run(context.Background(), Options{Ref: "localhost:5000/demo:" + tag, Source: fetchFailTarget{Store: inner, err: fetchErr}})
	if err == nil || !errors.Is(err, fetchErr) {
		t.Fatalf("err = %v, want the underlying fetch error", err)
	}
	if errors.Is(err, ErrMissingArtifact) || errors.Is(err, ErrNotAnArtifact) {
		t.Errorf("a fetch failure must not be classified as missing/not-an-artifact: %v", err)
	}
}

// layerFetchFailTarget serves the manifest but fails to fetch any layer of
// failType, standing in for a registry that loses a layer mid-pull.
type layerFetchFailTarget struct {
	*memory.Store
	failType string
	err      error
}

func (l layerFetchFailTarget) Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	if desc.MediaType == l.failType {
		return nil, l.err
	}
	return l.Store.Fetch(ctx, desc)
}

func TestRunLayerFetchErrorSurfaces(t *testing.T) {
	res, tag := mintArtifact(t, "")
	inner := memory.New()
	seedArtifact(t, inner, tag, freeze.ArtifactType, allLayers(res))
	fetchErr := errors.New("layer read reset")
	src := layerFetchFailTarget{Store: inner, failType: freeze.MediaTypeSignature, err: fetchErr}
	_, err := Run(context.Background(), Options{Ref: "localhost:5000/demo:" + tag, Source: src})
	if err == nil || !errors.Is(err, fetchErr) {
		t.Fatalf("err = %v, want the underlying layer fetch error", err)
	}
	if !strings.Contains(err.Error(), "fetch artifact layer") {
		t.Errorf("err = %v, want a fetch-artifact-layer message", err)
	}
}

func TestRunCorruptManifestErrors(t *testing.T) {
	// Tag a blob whose bytes are not a valid manifest JSON, so decode fails.
	st := memory.New()
	bad := []byte("{ this is not a manifest")
	// Use a non-manifest media type so the store does not reject the blob on
	// push; Resolve+FetchAll ignore media type, so the decode still runs on
	// these bytes and fails.
	d := content.NewDescriptorFromBytes("application/octet-stream", bad)
	if err := st.Push(context.Background(), d, bytes.NewReader(bad)); err != nil {
		t.Fatalf("push: %v", err)
	}
	if err := st.Tag(context.Background(), d, "v1abc"); err != nil {
		t.Fatalf("tag: %v", err)
	}
	_, err := Run(context.Background(), Options{Ref: "localhost:5000/demo:v1abc", Source: st})
	if err == nil || !strings.Contains(err.Error(), "decode artifact manifest") {
		t.Fatalf("err = %v, want a decode-artifact-manifest error", err)
	}
}

func TestRunWrongArtifactType(t *testing.T) {
	res, tag := mintArtifact(t, "")
	st := memory.New()
	seedArtifact(t, st, tag, "application/vnd.oci.image.manifest.v1+json", allLayers(res))
	_, err := Run(context.Background(), Options{Ref: "localhost:5000/demo:" + tag, Source: st})
	if !errors.Is(err, ErrNotAnArtifact) {
		t.Fatalf("err = %v, want ErrNotAnArtifact", err)
	}
}

// TestRunNoTagResolvesLatest is the ergonomics regression (#2027): an install
// ref with NO tag no longer errors; it defaults to the mutable `latest` tag
// publish always points at the newest artifact, and the on-disk id resolves to
// the content-hash slug carried in the manifest's version annotation.
func TestRunNoTagResolvesLatest(t *testing.T) {
	res, slug := mintArtifact(t, "")
	st := memory.New()
	// Publish points `latest` at the newest artifact; its version annotation is
	// the immutable content-hash slug.
	seedArtifactVersioned(t, st, "latest", slug, freeze.ArtifactType, allLayers(res))

	got, err := Run(context.Background(), Options{Ref: "localhost:5000/demo", Source: st})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Frozen.ID != slug {
		t.Errorf("frozen id = %q, want the resolved content-hash slug %q (not the moving tag)", got.Frozen.ID, slug)
	}
	if got.SourceTag != slug {
		t.Errorf("source tag = %q, want the resolved slug %q (launch resolves the image under it)", got.SourceTag, slug)
	}
}

// TestRunMovingTagKeysOffAnnotation proves a moving tag (latest or a semver
// label) whose manifest annotation carries a DISTINCT content-hash slug yields
// Frozen.ID and SourceTag equal to the annotation value, never the literal tag.
// Keying the on-disk id and persisted origin off the annotation is what keeps
// launch's `<slug>-image` resolution and the no-op re-install working.
func TestRunMovingTagKeysOffAnnotation(t *testing.T) {
	for _, movingTag := range []string{"latest", "1.4.2"} {
		res, slug := mintArtifact(t, "")
		st := memory.New()
		seedArtifactVersioned(t, st, movingTag, slug, freeze.ArtifactType, allLayers(res))

		got, err := Run(context.Background(), Options{Ref: "localhost:5000/demo:" + movingTag, Source: st})
		if err != nil {
			t.Fatalf("Run[%s]: %v", movingTag, err)
		}
		if got.Frozen.ID != slug {
			t.Errorf("[%s] frozen id = %q, want the annotation slug %q, not the moving tag", movingTag, got.Frozen.ID, slug)
		}
		if got.SourceTag != slug {
			t.Errorf("[%s] source tag = %q, want the annotation slug %q, not the moving tag", movingTag, got.SourceTag, slug)
		}
	}
}

// TestRunNoAnnotationFallsBackToTag proves a manifest without a version
// annotation (a pre-annotation or hand-built artifact) still installs, keying
// the id off the literal tag.
func TestRunNoAnnotationFallsBackToTag(t *testing.T) {
	res, tag := mintArtifact(t, "")
	st := memory.New()
	seedArtifactVersioned(t, st, tag, "", freeze.ArtifactType, allLayers(res)) // no annotation
	got, err := Run(context.Background(), Options{Ref: "localhost:5000/demo:" + tag, Source: st})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Frozen.ID != tag {
		t.Errorf("frozen id = %q, want the literal tag %q via fallback", got.Frozen.ID, tag)
	}
}

func TestRunDigestOnlyRefIsMissingVersionTag(t *testing.T) {
	digest := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	_, err := Run(context.Background(), Options{Ref: "localhost:5000/demo@" + digest, Source: memory.New()})
	if !errors.Is(err, ErrMissingVersionTag) {
		t.Fatalf("err = %v, want ErrMissingVersionTag", err)
	}
}

func TestRunMissingLayerIsMissingArtifact(t *testing.T) {
	res, tag := mintArtifact(t, "")
	layers := allLayers(res)
	delete(layers, freeze.MediaTypeSignature) // 3-layer manifest, signature absent
	st := memory.New()
	seedArtifact(t, st, tag, freeze.ArtifactType, layers)
	_, err := Run(context.Background(), Options{Ref: "localhost:5000/demo:" + tag, Source: st})
	if !errors.Is(err, ErrMissingArtifact) {
		t.Fatalf("err = %v, want ErrMissingArtifact", err)
	}
}

func TestRunTamperedSkillMDFailsVerify(t *testing.T) {
	res, tag := mintArtifact(t, "")
	layers := allLayers(res)
	// Flip a byte in the SKILL.md so the content hash no longer matches.
	tampered := append([]byte(nil), res.FrozenManifest...)
	tampered = append(tampered, byte('X'))
	layers[freeze.MediaTypeSkillMD] = tampered
	st := memory.New()
	seedArtifact(t, st, tag, freeze.ArtifactType, layers)
	_, err := Run(context.Background(), Options{Ref: "localhost:5000/demo:" + tag, Source: st})
	if err == nil {
		t.Fatal("tampered SKILL.md verified; want a verification error")
	}
	if errors.Is(err, ErrMissingArtifact) || errors.Is(err, ErrNotAnArtifact) {
		t.Fatalf("err = %v, want a VerifyFrozen failure", err)
	}
}

func TestRunUntrustedPublisherFailsClosed(t *testing.T) {
	res, tag := mintArtifact(t, "github://acme/plan")
	st := memory.New()
	seedArtifact(t, st, tag, freeze.ArtifactType, allLayers(res))
	refusal := errors.New("publisher not trusted")
	v := &recordingVerifier{err: refusal}
	_, err := Run(context.Background(), Options{Ref: "localhost:5000/demo:" + tag, Source: st, Verifier: v})
	if !errors.Is(err, refusal) {
		t.Fatalf("err = %v, want the verifier refusal", err)
	}
	if !v.called || v.publisher != "github://acme/plan" {
		t.Errorf("verifier called=%v publisher=%q, want called with github://acme/plan", v.called, v.publisher)
	}
}

func TestRunTrustedPublisherPasses(t *testing.T) {
	res, tag := mintArtifact(t, "github://acme/plan")
	st := memory.New()
	seedArtifact(t, st, tag, freeze.ArtifactType, allLayers(res))
	v := &recordingVerifier{} // trusts (nil err)
	got, err := Run(context.Background(), Options{Ref: "localhost:5000/demo:" + tag, Source: st, Verifier: v})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !v.called {
		t.Error("verifier was not called for a publisher-declaring plan")
	}
	if got.Verified.Publisher != "github://acme/plan" {
		t.Errorf("verified publisher = %q, want github://acme/plan", got.Verified.Publisher)
	}
}

func TestRunMalformedRefErrors(t *testing.T) {
	_, err := Run(context.Background(), Options{Ref: "not a ref!!", Source: memory.New()})
	if err == nil || !strings.Contains(err.Error(), "parse reference") {
		t.Fatalf("err = %v, want a parse-reference error", err)
	}
}

func TestRunNamelessSkillMDIsErrNoName(t *testing.T) {
	// A SKILL.md with valid frontmatter but no name cannot key a store
	// directory. skillName runs before VerifyFrozen, so this fails at the name
	// check regardless of signature.
	const nameless = "---\ndescription: no name here\n---\n\n# Body\n"
	res, tag := mintArtifact(t, "")
	layers := allLayers(res)
	layers[freeze.MediaTypeSkillMD] = []byte(nameless)
	st := memory.New()
	seedArtifact(t, st, tag, freeze.ArtifactType, layers)
	_, err := Run(context.Background(), Options{Ref: "localhost:5000/demo:" + tag, Source: st})
	if !errors.Is(err, ErrNoName) {
		t.Fatalf("err = %v, want ErrNoName", err)
	}
}

func TestRunUnparseableSkillMDErrors(t *testing.T) {
	// A SKILL.md with no frontmatter fails manifest.Parse before verification.
	res, tag := mintArtifact(t, "")
	layers := allLayers(res)
	layers[freeze.MediaTypeSkillMD] = []byte("no frontmatter at all\n")
	st := memory.New()
	seedArtifact(t, st, tag, freeze.ArtifactType, layers)
	_, err := Run(context.Background(), Options{Ref: "localhost:5000/demo:" + tag, Source: st})
	if err == nil || !strings.Contains(err.Error(), "parse pulled SKILL.md") {
		t.Fatalf("err = %v, want a parse-SKILL.md error", err)
	}
}

func TestRunPublisherlessSkipsGate(t *testing.T) {
	res, tag := mintArtifact(t, "") // no publisher
	st := memory.New()
	seedArtifact(t, st, tag, freeze.ArtifactType, allLayers(res))
	v := &recordingVerifier{err: errors.New("should not be consulted")}
	_, err := Run(context.Background(), Options{Ref: "localhost:5000/demo:" + tag, Source: st, Verifier: v})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if v.called {
		t.Error("verifier consulted for a publisher-less plan; the gate must be skipped")
	}
}
