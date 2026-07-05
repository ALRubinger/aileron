package freeze

import "testing"

func TestBindingKind(t *testing.T) {
	cases := []struct {
		name string
		pin  ImagePin
		want string
	}{
		{"composed pin binds by config digest", ImagePin{Digest: "sha256:aa", LocalTag: "aileron/sandbox-tools:x"}, BindingConfigDigest},
		{"image-only pin binds by manifest digest", ImagePin{Ref: "docker.io/library/python", Digest: "sha256:bb"}, BindingManifestDigest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BindingKind(tc.pin); got != tc.want {
				t.Errorf("BindingKind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseLockfileRoundTrip(t *testing.T) {
	in := Lockfile{
		ResolvedImages: []ImagePin{{Ref: "aileron/sandbox-tools", Digest: "sha256:abc", LocalTag: "aileron/sandbox-tools:x"}},
		Publisher:      "github://acme",
		ContentHash:    "sha256:deadbeef",
		Version:        "1.2.3",
	}
	data, err := MarshalLockfile(in)
	if err != nil {
		t.Fatalf("MarshalLockfile: %v", err)
	}
	got, err := ParseLockfile(data)
	if err != nil {
		t.Fatalf("ParseLockfile: %v", err)
	}
	if len(got.ResolvedImages) != 1 || got.ResolvedImages[0] != in.ResolvedImages[0] {
		t.Errorf("ResolvedImages round-trip = %+v, want %+v", got.ResolvedImages, in.ResolvedImages)
	}
	if got.Publisher != in.Publisher || got.Version != in.Version || got.ContentHash != in.ContentHash {
		t.Errorf("scalar fields round-trip = %+v, want %+v", got, in)
	}
}

func TestParseLockfileInvalid(t *testing.T) {
	if _, err := ParseLockfile([]byte("\tnot: [valid")); err == nil {
		t.Error("ParseLockfile on malformed YAML: want error, got nil")
	}
}
