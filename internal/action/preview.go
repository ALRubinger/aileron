package action

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ALRubinger/aileron/internal/sandbox"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// DefaultPreviewTimeout caps how long the runtime waits on a preview op
// before falling back to "Preview unavailable: timeout" per ADR-0016.
// Five seconds matches the ADR's suggested bound: long enough for a
// well-behaved upstream API, short enough that the user is not kept
// staring at a blank approval card.
const DefaultPreviewTimeout = 5 * time.Second

// PreviewInvoker is implemented by executors that can invoke an
// action's [approval.preview] directive ([ADR-0016]) ahead of the
// approval prompt. The runtime type-asserts the configured Executor to
// this interface in its action-run handler. Executors that do not
// implement it (e.g. StubExecutor in tests) cause the handler to skip
// preview and present the approval prompt with raw inputs only, which
// is the documented fallback when a preview cannot be obtained.
//
// [ADR-0016]: https://docs.withaileron.ai/adr/0016-approval-preview
type PreviewInvoker interface {
	// InvokePreview runs the manifest's [approval.preview] op against
	// the connector the gated action's first execute step targets,
	// using the same credential binding the gated op would receive.
	// Returns a [PreviewResult] whose Fields are populated on success
	// and whose Unavailable carries a short user-facing reason on
	// failure. Failures never propagate as Go errors; per ADR-0016 the
	// approval flow proceeds in every failure mode with the fallback
	// rendered alongside raw inputs.
	InvokePreview(ctx context.Context, m *Manifest, args map[string]any) PreviewResult
}

// PreviewResult is the rendered output of a single [approval.preview]
// invocation. Two terminal shapes:
//
//   - Success: Fields populated in manifest-declared order. Each entry
//     carries either a resolved string value or Missing=true (when
//     the render path did not exist in the response — the prompt
//     shows "n/a" for that label).
//   - Wholesale failure: Unavailable non-empty, Fields nil. The
//     approval prompt renders "Preview unavailable: <Unavailable>"
//     and the user can still decide.
//
// Mutually exclusive in practice: Unavailable populated only when the
// preview op call itself failed (HTTP non-2xx, timeout, sandbox
// denial, WASM trap) or the manifest declared no preview at all.
type PreviewResult struct {
	// Fields is the ordered list of {label, value} entries the prompt
	// renders. Order follows PreviewPolicy.RenderOrder so authors can
	// shape the prompt layout via the manifest's key sequence.
	Fields []PreviewField

	// Unavailable is the user-facing reason the preview op failed
	// wholesale. Empty when the preview ran successfully (even if some
	// individual fields had missing paths — those surface inline via
	// PreviewField.Missing).
	Unavailable string
}

// PreviewField is one rendered entry on the approval prompt.
type PreviewField struct {
	// Label is the user-facing key the manifest declared (e.g. "To").
	Label string

	// Value is the resolved string. Empty when Missing is true.
	Value string

	// Missing is true when the render path did not resolve in the
	// preview op's response. The UI surfaces these as "n/a" so the
	// user sees that the connector did not return that field, rather
	// than the field being silently absent.
	Missing bool

	// Multiline is true when the manifest's `multiline` directive named
	// this field's label. The approval surface renders these as
	// scrollable blockquotes below the inline rows rather than as
	// single-line `key: value` entries. Carries no semantic weight —
	// purely a presentation hint per ADR-0016.
	Multiline bool
}

// InvokePreview implements [PreviewInvoker]. Wraps the call in an
// `aileron.action.preview` span with attributes mirroring the
// action-execute span so spans + audit can be correlated by
// `aileron.action.name` and `aileron.connector.fqn`.
//
// Per ADR-0016, no failure mode is fatal: the method always returns a
// [PreviewResult] the caller can surface. The runtime never silently
// degrades to "no preview"; the user sees the failure reason and can
// decline.
func (e *SandboxExecutor) InvokePreview(ctx context.Context, m *Manifest, args map[string]any) PreviewResult {
	tracer := otel.GetTracerProvider().Tracer(tracerName)
	ctx, span := tracer.Start(ctx, "aileron.action.preview",
		trace.WithSpanKind(trace.SpanKindInternal),
	)
	defer span.End()

	if m == nil {
		span.SetStatus(codes.Error, "nil manifest")
		return PreviewResult{Unavailable: "internal error: nil manifest"}
	}
	span.SetAttributes(attribute.String("aileron.action.name", m.Name))

	preview := m.ApprovalPreview()
	if preview == nil {
		// Caller should have checked; treat as "no preview declared".
		return PreviewResult{}
	}
	if len(m.Execute) == 0 {
		return PreviewResult{Unavailable: "preview unavailable: action declares no execute step"}
	}
	connectorFQN := m.Execute[0].Connector
	span.SetAttributes(
		attribute.String("aileron.connector.fqn", connectorFQN),
		attribute.String("aileron.connector.op", preview.Op),
	)

	if e.Actions == nil || e.Store == nil || e.Runtime == nil {
		return PreviewResult{Unavailable: "preview unavailable: executor not fully configured"}
	}

	conn, connManifest, _, compileErr := e.connectorFor(ctx, m, connectorFQN)
	if compileErr != nil {
		span.SetStatus(codes.Error, compileErr.Error())
		return PreviewResult{Unavailable: previewReason(compileErr)}
	}

	previewArgs := InterpolatePreviewArgs(preview.Args, args)
	resolver := e.resolverFor(ctx, connManifest, connectorFQN)
	allowed := allowedAuthorityFor(m, connectorFQN)

	// Bound the upstream call so a hung connector cannot block the
	// approval prompt indefinitely. ADR-0016 suggests 5s; surface that
	// bound here rather than relying on a connector's own timeout
	// (which may be more generous than the human-attention budget).
	callCtx, cancel := context.WithTimeout(ctx, DefaultPreviewTimeout)
	defer cancel()

	res, invErr := e.invokeWithSpan(callCtx, conn, connectorFQN, preview.Op, "", sandbox.Call{
		Op:                 preview.Op,
		Args:               previewArgs,
		AllowedAuthority:   allowed,
		CredentialResolver: resolver,
	})
	if invErr != nil {
		if errors.Is(invErr, context.DeadlineExceeded) || errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			span.SetStatus(codes.Error, "preview timeout")
			return PreviewResult{Unavailable: "preview unavailable: timeout"}
		}
		span.SetStatus(codes.Error, invErr.Error())
		return PreviewResult{Unavailable: previewReason(invErr)}
	}

	return PreviewResult{Fields: extractPreviewFields(preview, res.Output)}
}

// extractPreviewFields walks the manifest's RenderOrder so the prompt
// renders labels in the author's intended order, looking each path up
// in `response` via ResolveFieldPath. A label whose path does not
// resolve surfaces as Missing=true so the UI can render "n/a"; per
// ADR-0016 the approval still proceeds.
//
// Falls back to a sorted-by-label iteration when RenderOrder is empty
// (older manifests parsed before order capture, or programmatic
// construction in tests). Order does not change semantics; this is a
// purely user-visible affordance.
func extractPreviewFields(preview *PreviewPolicy, response map[string]any) []PreviewField {
	labels := preview.RenderOrder
	if len(labels) == 0 {
		labels = make([]string, 0, len(preview.Render))
		for k := range preview.Render {
			labels = append(labels, k)
		}
		// Stable alphabetical fallback — deterministic for tests; the
		// ordered path above is the production case.
		sortStrings(labels)
	}
	multiline := make(map[string]bool, len(preview.Multiline))
	for _, l := range preview.Multiline {
		multiline[l] = true
	}
	out := make([]PreviewField, 0, len(labels))
	for _, label := range labels {
		path, ok := preview.Render[label]
		if !ok {
			continue
		}
		value, found := ResolveFieldPath(response, path)
		if !found {
			out = append(out, PreviewField{Label: label, Missing: true, Multiline: multiline[label]})
			continue
		}
		out = append(out, PreviewField{Label: label, Value: value, Multiline: multiline[label]})
	}
	return out
}

// previewReason formats an executor-side error as the user-facing
// fallback line the approval prompt renders. Keeps the prefix
// consistent ("preview unavailable: ...") so the UI can style or
// strip it uniformly.
func previewReason(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("preview unavailable: %s", err.Error())
}

// sortStrings is a tiny in-place insertion sort. Imported separately
// to keep this package's stdlib surface narrow; the slice is never
// large enough for sort.Strings to be worth the import cost.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
