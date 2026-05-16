package action

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// semverRe matches strict SemVer 2.0.0 (no leading "v"), per ADR-0002 and
// ADR-0003. Pre-release and build-metadata tails are accepted.
var semverRe = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

// nameRe validates the action's bare local handle. Per ADR-0003 the user
// owns the file and chooses the name; we constrain it to a portable filename
// shape so listing endpoints and on-disk discovery agree.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// inputNameRe validates an action input's identifier. Inputs become
// JSON Schema property names exposed to the LLM, so they're tighter
// than the action's own name: lowercase snake_case, must start with a
// letter.
var inputNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// inputTypes is the closed set of JSON Schema types accepted in an
// `[[inputs]]` block. Scalar types map directly to the LLM-facing tool
// schema. Structured types (`array`, `object`) are passed through with
// no per-property constraints in v1; the connector is responsible for
// validating semantic shape at op time. Arrays may declare an element
// shape via `items_type` (see inputItemsTypes).
var inputTypes = map[string]bool{
	"string":  true,
	"integer": true,
	"number":  true,
	"boolean": true,
	"array":   true,
	"object":  true,
}

// inputItemsTypes is the closed set of element types accepted in an
// `[[inputs]]` block's `items_type`. Same v1 primitives the top-level
// `type` accepts, with one exception: `array` is omitted so the
// schema deriver doesn't have to reason about nested arrays without a
// way to express their own element shape.
var inputItemsTypes = map[string]bool{
	"string":  true,
	"integer": true,
	"number":  true,
	"boolean": true,
	"object":  true,
}

// fqnSchemes is the closed set of FQN schemes recognized in v1, per ADR-0002.
// Adding a remote-resolver scheme requires an ADR amendment.
//
// The `local` scheme is the exception: it names BYOCLI connectors
// authored by `aileron cli add` / `aileron pp add` (issue #749).
// Their action.md `source` fields shape as `local://user/<name>/<op>@<version>`;
// without `local` in this allowlist the action loader rejects
// every BYOCLI-emitted action with `invalid source: scheme "local"
// is not recognized`. Mirrors the same entry in
// [cstore.fqnSchemes].
var fqnSchemes = map[string]bool{
	"github": true,
	"gitlab": true,
	"hub":    true,
	"local":  true,
}

// Validate checks a parsed manifest against the schema described in
// ADR-0001 and ADR-0003. Returns a *Error on the first failure. Successive
// fields are checked in declared order so authors see one specific message
// at a time rather than a flood.
func Validate(m *Manifest, file string) error {
	if m == nil {
		return newValidationErr(file, "manifest is nil")
	}
	if m.Name == "" {
		return newValidationErr(file, "name is required")
	}
	if !nameRe.MatchString(m.Name) {
		return newValidationErr(file, "name %q must match %s", m.Name, nameRe.String())
	}
	if m.Version == "" {
		return newValidationErr(file, "version is required")
	}
	if !semverRe.MatchString(m.Version) {
		return newValidationErr(file, "version %q must be strict SemVer (e.g. 1.2.3)", m.Version)
	}
	if m.Source == "" {
		return newValidationErr(file, "source is required")
	}
	if err := validateSourceFQN(m.Source); err != nil {
		return newValidationErr(file, "invalid source: %s", err.Error())
	}
	if len(m.Requires.Connectors) == 0 {
		return newValidationErr(file, "[[requires.connectors]] is required (at least one connector)")
	}
	for i, c := range m.Requires.Connectors {
		if c.Name == "" {
			return newValidationErr(file, "requires.connectors[%d].name is required", i)
		}
		if err := validateConnectorFQN(c.Name); err != nil {
			return newValidationErr(file, "requires.connectors[%d].name: %s", i, err.Error())
		}
		if c.Version == "" {
			return newValidationErr(file, "requires.connectors[%d].version is required", i)
		}
		if !semverRe.MatchString(c.Version) {
			return newValidationErr(file, "requires.connectors[%d].version %q must be strict SemVer", i, c.Version)
		}
		if c.Hash == "" {
			return newValidationErr(file, "requires.connectors[%d].hash is required", i)
		}
		if !strings.HasPrefix(c.Hash, "sha256:") || len(c.Hash) <= len("sha256:") {
			return newValidationErr(file, "requires.connectors[%d].hash %q must be prefixed with sha256:", i, c.Hash)
		}
		if len(c.Capabilities) == 0 {
			return newValidationErr(file, "requires.connectors[%d].capabilities is required (declare the subset the action exercises)", i)
		}
		for j, cap := range c.Capabilities {
			if strings.TrimSpace(cap) == "" {
				return newValidationErr(file, "requires.connectors[%d].capabilities[%d] is empty", i, j)
			}
		}
	}
	if m.Match.Intent == "" {
		return newValidationErr(file, "[match].intent is required")
	}
	inputNames := map[string]bool{}
	for i, in := range m.Inputs {
		if in.Name == "" {
			return newValidationErr(file, "inputs[%d].name is required", i)
		}
		if !inputNameRe.MatchString(in.Name) {
			return newValidationErr(file, "inputs[%d].name %q must match %s", i, in.Name, inputNameRe.String())
		}
		if inputNames[in.Name] {
			return newValidationErr(file, "inputs[%d].name %q is duplicated", i, in.Name)
		}
		inputNames[in.Name] = true
		if in.Type == "" {
			return newValidationErr(file, "inputs[%d].type is required", i)
		}
		if !inputTypes[in.Type] {
			return newValidationErr(file, "inputs[%d].type %q must be one of string|integer|number|boolean|array|object", i, in.Type)
		}
		if strings.TrimSpace(in.Description) == "" {
			return newValidationErr(file, "inputs[%d].description is required (this is what the LLM sees)", i)
		}
		// Label is optional, but when set must be non-blank — a label
		// of all whitespace is a footgun the approval surface would
		// render as an empty header.
		if in.Label != "" && strings.TrimSpace(in.Label) == "" {
			return newValidationErr(file, "inputs[%d].label is whitespace-only", i)
		}
		// Multiline is a presentation hint for long-form text. Other
		// JSON-Schema primitives have no scrollable-block surface, so
		// allowing it on non-string inputs would silently mislead
		// manifest authors. Reject at load time.
		if in.Multiline && in.Type != "string" {
			return newValidationErr(file,
				"inputs[%d].multiline=true is only allowed when type=\"string\" (got %q)", i, in.Type)
		}
		// ItemsType declares an array's element shape. It is only
		// meaningful on array inputs; rejecting on non-arrays catches
		// manifest typos rather than silently ignoring the field.
		if in.ItemsType != "" {
			if in.Type != "array" {
				return newValidationErr(file,
					"inputs[%d].items_type is only allowed when type=\"array\" (got %q)", i, in.Type)
			}
			if !inputItemsTypes[in.ItemsType] {
				return newValidationErr(file,
					"inputs[%d].items_type %q must be one of string|integer|number|boolean|object", i, in.ItemsType)
			}
		}
	}
	if len(m.Execute) == 0 {
		return newValidationErr(file, "[[execute]] is required (at least one step)")
	}
	connectorByFQN := map[string]bool{}
	for _, c := range m.Requires.Connectors {
		connectorByFQN[c.Name] = true
	}
	if err := validatePreview(m, file, inputNames); err != nil {
		return err
	}
	stepIDs := map[string]bool{}
	for i, s := range m.Execute {
		if s.ID == "" {
			return newValidationErr(file, "execute[%d].id is required", i)
		}
		if stepIDs[s.ID] {
			return newValidationErr(file, "execute[%d].id %q is duplicated", i, s.ID)
		}
		stepIDs[s.ID] = true
		if s.Connector == "" {
			return newValidationErr(file, "execute[%d].connector is required", i)
		}
		if !connectorByFQN[s.Connector] {
			return newValidationErr(file, "execute[%d].connector %q is not declared in [[requires.connectors]]", i, s.Connector)
		}
		if s.Op == "" {
			return newValidationErr(file, "execute[%d].op is required", i)
		}
		for k, v := range s.Inputs {
			refs := argsRefs(v)
			for _, ref := range refs {
				if !inputNames[ref] {
					return newValidationErr(file,
						"execute[%d].inputs[%q] references ${args.%s} but no [[inputs]] block declares %q",
						i, k, ref, ref)
				}
			}
		}
	}
	return nil
}

// validatePreview enforces the per-manifest [approval.preview] rules
// from [ADR-0016]:
//
//   - `op` is required and non-empty.
//   - `render` is required and non-empty.
//   - every `${args.X}` reference in `args` resolves to a declared
//     [[inputs]] entry on the gated action.
//
// Cross-manifest rules (preview op declared idempotent in some action
// manifest, preview op not itself approval-gated) are enforced by
// validatePreviewBundle at store-load time. The "op exists on the same
// connector" rule is enforced by the connector at runtime: an op the
// connector does not implement returns a structured error that the
// runtime surfaces as the "Preview unavailable" fallback.
//
// Returns nil when the manifest declares no [approval.preview] block,
// matching the opt-in nature of the directive.
//
// [ADR-0016]: https://docs.withaileron.ai/adr/0016-approval-preview
func validatePreview(m *Manifest, file string, inputNames map[string]bool) error {
	preview := m.ApprovalPreview()
	if preview == nil {
		return nil
	}
	if strings.TrimSpace(preview.Op) == "" {
		return newValidationErr(file, "[approval.preview].op is required")
	}
	if len(preview.Render) == 0 {
		return newValidationErr(file, "[approval.preview].render must declare at least one field")
	}
	for k, v := range preview.Args {
		refs := argsRefs(v)
		for _, ref := range refs {
			if !inputNames[ref] {
				return newValidationErr(file,
					"[approval.preview].args[%q] references ${args.%s} but no [[inputs]] block declares %q",
					k, ref, ref)
			}
		}
	}
	for label, path := range preview.Render {
		if strings.TrimSpace(label) == "" {
			return newValidationErr(file, "[approval.preview].render label is empty")
		}
		if strings.TrimSpace(path) == "" {
			return newValidationErr(file,
				"[approval.preview].render[%q] path is empty", label)
		}
	}
	for _, label := range preview.Multiline {
		if _, ok := preview.Render[label]; !ok {
			return newValidationErr(file,
				"[approval.preview].multiline references %q but no matching key exists in [approval.preview].render",
				label)
		}
	}
	return nil
}

// argsRe matches `${args.NAME}` interpolation references inside an
// execute step's input value. The captured group is the argument name.
var argsRe = regexp.MustCompile(`\$\{args\.([a-z][a-z0-9_]*)\}`)

// argsRefs walks an execute step input value (which TOML decodes to
// any of: string, []any, map[string]any) and returns every
// `${args.NAME}` reference it finds.
func argsRefs(v any) []string {
	var out []string
	switch val := v.(type) {
	case string:
		for _, m := range argsRe.FindAllStringSubmatch(val, -1) {
			out = append(out, m[1])
		}
	case []any:
		for _, item := range val {
			out = append(out, argsRefs(item)...)
		}
	case map[string]any:
		for _, item := range val {
			out = append(out, argsRefs(item)...)
		}
	}
	return out
}

// validatePreviewBundle enforces the cross-manifest [approval.preview]
// rules from [ADR-0016] that depend on inspecting other action
// manifests in the bundle:
//
//   - The preview op must be declared `idempotent = true` in at least
//     one bundled action manifest that targets the same connector.
//   - The preview op must NOT itself appear as an approval-gated op in
//     any bundled action manifest (no preview-of-preview recursion).
//
// `bundle` is every action manifest the store has loaded. For each
// preview directive, the function walks the bundle once and reports
// the first failing rule. Returns a per-file *Error so the caller
// (the store's Load) can aggregate alongside parse and per-manifest
// validation errors.
//
// [ADR-0016]: https://docs.withaileron.ai/adr/0016-approval-preview
func validatePreviewBundle(bundle map[string]LoadedAction) []*Error {
	type opKey struct {
		connector string
		op        string
	}
	idempotentOps := map[opKey]bool{}
	approvalOps := map[opKey]bool{}
	for _, la := range bundle {
		m := la.Manifest
		gated := m.ApprovalRequired()
		for _, step := range m.Execute {
			key := opKey{connector: step.Connector, op: step.Op}
			if step.Idempotent != nil && *step.Idempotent {
				idempotentOps[key] = true
			}
			if gated {
				approvalOps[key] = true
			}
		}
	}
	var out []*Error
	for _, la := range bundle {
		preview := la.Manifest.ApprovalPreview()
		if preview == nil {
			continue
		}
		gatedConnector := ""
		if len(la.Manifest.Execute) > 0 {
			gatedConnector = la.Manifest.Execute[0].Connector
		}
		if gatedConnector == "" {
			out = append(out, newValidationErr(la.Path,
				"[approval.preview] requires the gated action to declare at least one [[execute]] step (to resolve the preview op's connector)"))
			continue
		}
		key := opKey{connector: gatedConnector, op: preview.Op}
		if !idempotentOps[key] {
			out = append(out, newValidationErr(la.Path,
				"[approval.preview].op %q is not declared `idempotent = true` in any bundled action manifest targeting connector %q",
				preview.Op, gatedConnector))
			continue
		}
		if approvalOps[key] {
			out = append(out, newValidationErr(la.Path,
				"[approval.preview].op %q is gated by [approval] required = true in a bundled action manifest; preview ops must be safe to invoke without approval",
				preview.Op))
			continue
		}
	}
	return out
}

// validateSourceFQN parses an action source URI of the shape
// `<scheme>://<owner>/<path>@<version>`. The version suffix is mandatory for
// `source` per ADR-0003.
func validateSourceFQN(s string) error {
	at := strings.LastIndex(s, "@")
	if at < 0 {
		return fmt.Errorf("missing @<version> suffix")
	}
	base, ver := s[:at], s[at+1:]
	if !semverRe.MatchString(ver) {
		return fmt.Errorf("version %q must be strict SemVer", ver)
	}
	return validateConnectorFQN(base)
}

// validateConnectorFQN parses a connector URI of the shape
// `<scheme>://<owner>/<path>` (no version suffix — the version pins live in
// adjacent fields).
func validateConnectorFQN(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("not a valid URI: %s", err.Error())
	}
	if !fqnSchemes[u.Scheme] {
		schemes := make([]string, 0, len(fqnSchemes))
		for k := range fqnSchemes {
			schemes = append(schemes, k+"://")
		}
		return fmt.Errorf("scheme %q is not recognized (expected one of %s)", u.Scheme, strings.Join(schemes, ", "))
	}
	if u.Host == "" {
		return fmt.Errorf("missing owner segment")
	}
	if u.Path == "" || u.Path == "/" {
		return fmt.Errorf("missing path segment")
	}
	return nil
}
