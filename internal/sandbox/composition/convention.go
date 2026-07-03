package composition

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ALRubinger/aileron/internal/credential/inject"
)

// This file defines the credential-convention loader for tool catalog entries
// (the devcontainer Features under images/sandbox-features/<toolID>/). A
// credential-carrying Feature declares, under
// customizations.aileron.credential, (1) which ADR-0019 injection scheme seals
// its outbound requests and (2) the non-secret placeholder pair(s) the launcher
// plants into the sandbox environment so the proxy can recognize and re-sign or
// swap requests at the network boundary.
//
// The block is non-secret by construction: a placeholder carries a
// format-mimicking value (e.g. an AWS access key ID shaped like AKIA…) and the
// env var it is planted under, never real authority. The host injects the real
// secret at the network boundary; the placeholder only lets the proxy match the
// request that needs sealing. This mirrors the proxybinding.Sentinel posture,
// where the placeholder value and its plant location are catalog data rather
// than Go constants.
//
// The vocabulary (scheme, placeholders' env + value) is lifted verbatim from
// the existing sealing/sentinel vocabulary used by internal/proxybinding and
// gh's customizations.aileron.cli block, so a catalog entry expresses its
// convention in one shared shape rather than a parallel invented one.

// CredentialPlaceholder is a single non-secret placeholder the launcher plants
// into the sandbox environment. Value is a format-mimicking, authority-free
// stand-in (e.g. a well-known example access key ID) and Env is the environment
// variable it is planted under. Both fields carry the same names and semantics
// as the sentinel-swap vocabulary in proxybinding and gh's cli block.
type CredentialPlaceholder struct {
	Env   string `json:"env"`
	Value string `json:"value"`
}

// CredentialConvention is the typed, validated credential convention a catalog
// entry declares. Scheme is a member of the ADR-0019 closed set (routed through
// [inject.ParseScheme]); Placeholders lists the non-secret env/value pairs the
// launcher plants. It is the parse result freeze/launch consumers depend on.
type CredentialConvention struct {
	// Scheme names how the resolved secret is bound onto the outbound request
	// at the network boundary. It is always a valid [inject.Scheme].
	Scheme inject.Scheme
	// Placeholders are the non-secret env/value pairs the launcher plants. At
	// least one is present, each has a non-empty env and value, and no two
	// share an env.
	Placeholders []CredentialPlaceholder
}

// featureCredentialEnvelope decodes only the customizations.aileron.credential
// path of a devcontainer-feature.json. Every level is a pointer so a missing
// block (agent Features carry none) is distinguishable from a present-but-empty
// one: a nil credential pointer is the clean "no convention" result, while a
// present block is validated and any malformed shape is a loud error.
type featureCredentialEnvelope struct {
	Customizations *struct {
		Aileron *struct {
			// Credential captures the raw bytes of the credential block so the
			// strict second pass decodes the original JSON (rejecting unknown
			// keys) rather than a re-encoding. A nil RawMessage means the key
			// was absent; a present key is strict-decoded and validated.
			Credential *json.RawMessage `json:"credential"`
		} `json:"aileron"`
	} `json:"customizations"`
}

// credentialBlock is the raw customizations.aileron.credential sub-document.
// Decoding of this block is strict (unknown keys rejected) so a typo inside a
// present convention fails fast rather than silently shipping a partial
// convention, mirroring cli.Parse's KnownFields posture.
type credentialBlock struct {
	Scheme       string                  `json:"scheme"`
	Placeholders []CredentialPlaceholder `json:"placeholders"`
}

// ErrInvalidCredentialConvention is the sentinel wrapping every validation
// failure of a present-but-malformed credential block (missing scheme, no
// placeholders, a placeholder missing env or value, or a duplicate env).
// Callers pattern-match with errors.Is. An unknown scheme additionally wraps
// [inject.ErrUnknownScheme] so callers can distinguish that case.
var ErrInvalidCredentialConvention = errors.New("composition: invalid credential convention")

// ParseCredentialConvention parses a catalog entry's credential convention from
// its raw devcontainer-feature.json bytes. It is pure and byte-driven so
// freeze/launch consumers can feed manifest bytes from whatever source they
// resolve (repo checkout, image metadata, published Feature manifest).
//
// The return contract:
//   - No customizations.aileron.credential block: (zero, false, nil). Agent
//     Features (claude, codex) carry no convention and take this path.
//   - A present, valid block: (convention, true, nil).
//   - Malformed JSON, or a present-but-invalid block: (zero, false, err). A
//     present-but-broken convention is a loud error, never a silent no-op.
//
// Validation is fail-closed: scheme is required and routed through
// [inject.ParseScheme] (an unknown scheme wraps [inject.ErrUnknownScheme]); at
// least one placeholder is required; every placeholder needs a non-empty env
// and value; and no two placeholders may share an env. Unknown keys inside the
// credential block are rejected.
func ParseCredentialConvention(manifest []byte) (CredentialConvention, bool, error) {
	// First pass: locate the credential block with a lenient decode. Unknown
	// keys elsewhere in the manifest (the full devcontainer schema, the cli
	// sibling block) are irrelevant to this loader and must not fail it.
	var env featureCredentialEnvelope
	if err := json.Unmarshal(manifest, &env); err != nil {
		return CredentialConvention{}, false, fmt.Errorf("composition: parse credential convention: %w", err)
	}
	if env.Customizations == nil ||
		env.Customizations.Aileron == nil ||
		env.Customizations.Aileron.Credential == nil {
		return CredentialConvention{}, false, nil
	}

	// Second pass: strict-decode the credential block's original bytes so a typo
	// inside a present convention (e.g. "placeholder" for "placeholders") fails
	// fast.
	dec := json.NewDecoder(bytes.NewReader(*env.Customizations.Aileron.Credential))
	dec.DisallowUnknownFields()
	var block credentialBlock
	if err := dec.Decode(&block); err != nil {
		return CredentialConvention{}, false, fmt.Errorf("%w: %v", ErrInvalidCredentialConvention, err)
	}

	conv, err := block.validate()
	if err != nil {
		return CredentialConvention{}, false, err
	}
	return conv, true, nil
}

// validate applies the fail-closed rules to a decoded credential block and
// returns the typed convention. Every failure wraps
// [ErrInvalidCredentialConvention]; an unknown scheme additionally wraps
// [inject.ErrUnknownScheme].
func (b credentialBlock) validate() (CredentialConvention, error) {
	if b.Scheme == "" {
		return CredentialConvention{}, fmt.Errorf("%w: scheme is required", ErrInvalidCredentialConvention)
	}
	scheme, err := inject.ParseScheme(b.Scheme)
	if err != nil {
		// Wrap both sentinels: callers can errors.Is either
		// ErrInvalidCredentialConvention (any bad convention) or
		// inject.ErrUnknownScheme (specifically an unknown scheme).
		return CredentialConvention{}, fmt.Errorf("%w: %w", ErrInvalidCredentialConvention, err)
	}
	if len(b.Placeholders) == 0 {
		return CredentialConvention{}, fmt.Errorf("%w: at least one placeholder is required", ErrInvalidCredentialConvention)
	}
	seen := make(map[string]struct{}, len(b.Placeholders))
	for i, p := range b.Placeholders {
		if p.Env == "" {
			return CredentialConvention{}, fmt.Errorf("%w: placeholder %d missing env", ErrInvalidCredentialConvention, i)
		}
		if p.Value == "" {
			return CredentialConvention{}, fmt.Errorf("%w: placeholder %q missing value", ErrInvalidCredentialConvention, p.Env)
		}
		if _, dup := seen[p.Env]; dup {
			return CredentialConvention{}, fmt.Errorf("%w: duplicate placeholder env %q", ErrInvalidCredentialConvention, p.Env)
		}
		seen[p.Env] = struct{}{}
	}

	placeholders := make([]CredentialPlaceholder, len(b.Placeholders))
	copy(placeholders, b.Placeholders)
	return CredentialConvention{Scheme: scheme, Placeholders: placeholders}, nil
}

// LoadCredentialConvention reads <featuresRoot>/<toolID>/devcontainer-feature.json
// and parses its credential convention via [ParseCredentialConvention]. It is
// the filesystem-backed entry point for consumers that resolve catalog entries
// from a repo checkout; consumers that already hold manifest bytes call
// [ParseCredentialConvention] directly. The (convention, ok, err) contract is
// identical to [ParseCredentialConvention]; a missing manifest file is an
// error.
func LoadCredentialConvention(featuresRoot, toolID string) (CredentialConvention, bool, error) {
	path := filepath.Join(featuresRoot, toolID, "devcontainer-feature.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return CredentialConvention{}, false, fmt.Errorf("composition: read %s: %w", path, err)
	}
	return ParseCredentialConvention(raw)
}
