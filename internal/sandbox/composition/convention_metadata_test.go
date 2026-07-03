package composition

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/credential/inject"
)

// awsCredentialElement is one devcontainer.metadata array element carrying the
// aws-cli credential convention (sigv4-resign + the AWS placeholder pair),
// byte-shaped like the real Feature manifest.
const awsCredentialElement = `{
  "id": "aws-cli",
  "customizations": {
    "aileron": {
      "credential": {
        "scheme": "sigv4-resign",
        "placeholders": [
          {"env": "AWS_ACCESS_KEY_ID", "value": "AKIAIOSFODNN7PLACEHLDR"},
          {"env": "AWS_SECRET_ACCESS_KEY", "value": "placeholderAileronInjectsRealSecretXXXXXX"}
        ]
      }
    }
  }
}`

// ghCredentialElement carries gh's credential convention (bearer + the GH_TOKEN
// placeholder).
const ghCredentialElement = `{
  "id": "gh",
  "customizations": {
    "aileron": {
      "credential": {
        "scheme": "bearer",
        "placeholders": [
          {"env": "GH_TOKEN", "value": "ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA"}
        ]
      }
    }
  }
}`

// agentElement is a metadata element with no credential block (an agent
// Feature): it must contribute nothing.
const agentElement = `{"id": "claude", "customizations": {"aileron": {"cli": {"name": "gh"}}}}`

func TestConventionsFromMetadata_EmptyAndWhitespace(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\t ", "[]"} {
		convs, err := ConventionsFromMetadata([]byte(in))
		if err != nil {
			t.Fatalf("ConventionsFromMetadata(%q): unexpected error %v", in, err)
		}
		if convs != nil {
			t.Fatalf("ConventionsFromMetadata(%q) = %v, want nil", in, convs)
		}
	}
}

func TestConventionsFromMetadata_TwoElementsInOrder(t *testing.T) {
	metadata := []byte("[" + awsCredentialElement + "," + ghCredentialElement + "]")
	convs, err := ConventionsFromMetadata(metadata)
	if err != nil {
		t.Fatalf("ConventionsFromMetadata: %v", err)
	}
	if len(convs) != 2 {
		t.Fatalf("got %d conventions, want 2", len(convs))
	}
	if convs[0].Scheme != inject.SchemeSigV4Resign {
		t.Fatalf("convs[0].Scheme = %q, want sigv4-resign", convs[0].Scheme)
	}
	if convs[1].Scheme != inject.SchemeBearer {
		t.Fatalf("convs[1].Scheme = %q, want bearer", convs[1].Scheme)
	}
}

func TestConventionsFromMetadata_ElementWithoutCredentialContributesNothing(t *testing.T) {
	metadata := []byte("[" + agentElement + "," + ghCredentialElement + "]")
	convs, err := ConventionsFromMetadata(metadata)
	if err != nil {
		t.Fatalf("ConventionsFromMetadata: %v", err)
	}
	if len(convs) != 1 {
		t.Fatalf("got %d conventions, want 1 (the agent element contributes nothing)", len(convs))
	}
	if convs[0].Scheme != inject.SchemeBearer {
		t.Fatalf("convs[0].Scheme = %q, want bearer", convs[0].Scheme)
	}
}

func TestConventionsFromMetadata_NonArrayIsError(t *testing.T) {
	if _, err := ConventionsFromMetadata([]byte(awsCredentialElement)); err == nil {
		t.Fatal("expected error for non-array JSON, got nil")
	}
}

func TestConventionsFromMetadata_PresentButInvalidBlockIsError(t *testing.T) {
	unknownScheme := `[{"customizations":{"aileron":{"credential":{"scheme":"not-a-scheme","placeholders":[{"env":"X","value":"y"}]}}}}]`
	_, err := ConventionsFromMetadata([]byte(unknownScheme))
	if err == nil {
		t.Fatal("expected error for unknown scheme, got nil")
	}
	if !errors.Is(err, ErrInvalidCredentialConvention) {
		t.Fatalf("error %v does not wrap ErrInvalidCredentialConvention", err)
	}

	zeroPlaceholders := `[{"customizations":{"aileron":{"credential":{"scheme":"bearer","placeholders":[]}}}}]`
	if _, err := ConventionsFromMetadata([]byte(zeroPlaceholders)); err == nil {
		t.Fatal("expected error for zero placeholders, got nil")
	}
}

func TestPlaceholderEnv_UnionOfAWSAndGH(t *testing.T) {
	metadata := []byte("[" + awsCredentialElement + "," + ghCredentialElement + "]")
	convs, err := ConventionsFromMetadata(metadata)
	if err != nil {
		t.Fatalf("ConventionsFromMetadata: %v", err)
	}
	env, err := PlaceholderEnv(convs)
	if err != nil {
		t.Fatalf("PlaceholderEnv: %v", err)
	}
	want := map[string]string{
		"AWS_ACCESS_KEY_ID":     "AKIAIOSFODNN7PLACEHLDR",
		"AWS_SECRET_ACCESS_KEY": "placeholderAileronInjectsRealSecretXXXXXX",
		"GH_TOKEN":              "ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA",
	}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("PlaceholderEnv = %v, want %v", env, want)
	}
}

func TestPlaceholderEnv_NilInput(t *testing.T) {
	env, err := PlaceholderEnv(nil)
	if err != nil {
		t.Fatalf("PlaceholderEnv(nil): %v", err)
	}
	if env != nil {
		t.Fatalf("PlaceholderEnv(nil) = %v, want nil", env)
	}
}

func TestPlaceholderEnv_DuplicateIdenticalDedupes(t *testing.T) {
	conv := CredentialConvention{
		Scheme: inject.SchemeBearer,
		Placeholders: []CredentialPlaceholder{
			{Env: "GH_TOKEN", Value: "ghp_x"},
		},
	}
	env, err := PlaceholderEnv([]CredentialConvention{conv, conv})
	if err != nil {
		t.Fatalf("PlaceholderEnv: %v", err)
	}
	want := map[string]string{"GH_TOKEN": "ghp_x"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("PlaceholderEnv = %v, want %v", env, want)
	}
}

func TestPlaceholderEnv_ConflictingValuesError(t *testing.T) {
	convA := CredentialConvention{
		Scheme:       inject.SchemeBearer,
		Placeholders: []CredentialPlaceholder{{Env: "GH_TOKEN", Value: "ghp_a"}},
	}
	convB := CredentialConvention{
		Scheme:       inject.SchemeBearer,
		Placeholders: []CredentialPlaceholder{{Env: "GH_TOKEN", Value: "ghp_b"}},
	}
	_, err := PlaceholderEnv([]CredentialConvention{convA, convB})
	if err == nil {
		t.Fatal("expected error for conflicting placeholder values, got nil")
	}
	if !strings.Contains(err.Error(), "GH_TOKEN") {
		t.Fatalf("error %v does not name the conflicting env GH_TOKEN", err)
	}
}

// TestPlaceholderEnv_RealCatalogUnion loads the actual repo manifests through
// LoadCredentialConvention (the aws-cli + gh multi-tool union the brief
// requires) and asserts PlaceholderEnv yields both conventions' placeholders
// byte-for-byte and nothing else, pinned to real catalog data.
func TestPlaceholderEnv_RealCatalogUnion(t *testing.T) {
	root := featuresContext(t)

	aws, ok, err := LoadCredentialConvention(root, "aws-cli")
	if err != nil || !ok {
		t.Fatalf("LoadCredentialConvention(aws-cli): ok=%v err=%v", ok, err)
	}
	gh, ok, err := LoadCredentialConvention(root, "gh")
	if err != nil || !ok {
		t.Fatalf("LoadCredentialConvention(gh): ok=%v err=%v", ok, err)
	}

	env, err := PlaceholderEnv([]CredentialConvention{aws, gh})
	if err != nil {
		t.Fatalf("PlaceholderEnv: %v", err)
	}
	want := map[string]string{
		"AWS_ACCESS_KEY_ID":     "AKIAIOSFODNN7PLACEHLDR",
		"AWS_SECRET_ACCESS_KEY": "placeholderAileronInjectsRealSecretXXXXXX",
		"GH_TOKEN":              "ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA",
	}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("real-catalog PlaceholderEnv = %v, want %v", env, want)
	}
}
