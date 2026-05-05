package observability

import (
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// tracerName is the instrumentation library name reported on the
// HTTP server-root spans this middleware emits. Distinct from
// resource service.name (which identifies Aileron itself); this
// names the *instrumentation* (per OTel conventions), not the
// service.
const tracerName = "github.com/ALRubinger/aileron/internal/observability"

// HTTPMiddleware returns an HTTP middleware that:
//
//  1. Extracts incoming W3C TraceContext (`traceparent` /
//     `tracestate`) into the request context — when an upstream
//     caller passed one, downstream spans become children of that
//     trace and the agent's end-to-end view stays coherent.
//  2. Starts an HTTP server-root span named per the supplied
//     spanNameFn. The span ends when the handler returns; status
//     defaults to OK.
//
// When tracing is in no-op mode (the daemon-startup default) the
// extraction is still performed — propagation is cheap, useful, and
// orthogonal to whether *this* process emits — and the no-op
// tracer's Start returns a span whose End is a no-op. Net runtime
// cost: a header lookup and a few interface calls per request.
//
// spanNameFn shapes the span's name from the request. Callers pass a
// fixed-string returner like `"aileron.actions.run"` or a templater
// that emits `"GET /v1/actions"`. Keeping it a function lets the
// gateway-handler-specific names land in their respective handlers
// without leaking handler details into this package.
func HTTPMiddleware(spanNameFn func(*http.Request) string) func(http.Handler) http.Handler {
	if spanNameFn == nil {
		spanNameFn = defaultSpanName
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))

			tracer := otel.GetTracerProvider().Tracer(tracerName)
			ctx, span := tracer.Start(ctx, spanNameFn(r),
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					attribute.String("http.method", r.Method),
					attribute.String("http.target", r.URL.Path),
				),
			)
			defer span.End()

			// Wrap the response writer so the handler's status code can
			// be reflected on the span. Span status mirrors HTTP status:
			// 5xx → Error, others → Ok (the OTel convention).
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r.WithContext(ctx))

			span.SetAttributes(attribute.Int("http.status_code", rec.status))
			if rec.status >= 500 {
				span.SetStatus(codes.Error, http.StatusText(rec.status))
			}
		})
	}
}

func defaultSpanName(r *http.Request) string {
	return r.Method + " " + r.URL.Path
}

// statusRecorder wraps http.ResponseWriter to capture the status
// code the handler chose. Without this, a handler that calls
// w.WriteHeader(500) leaves the middleware unable to mark the span
// as failed.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true
		// status stays at the default 200 the constructor set
	}
	return s.ResponseWriter.Write(b)
}
