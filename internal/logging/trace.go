package logging

import (
	"context"
	"log/slog"
)

// The trace-correlation carrier — the half of decisions/401 that was decided
// and never built.
//
// # Why the carrier lives here and not in the tracing package
//
// d-401 admits exactly one thing into context.Context for this purpose: "log
// fields", consumer `logging`. Reading the OTel span out of ctx directly in
// the handler would be the idiomatic OTel route and is the wrong one here —
// `go list -deps ./internal/logging` returns only itself, and 78 packages
// import it, so that one import would put go.opentelemetry.io/otel/trace into
// every dependency graph in the tree to render two hex strings. The carrier
// is instead self-contained, exactly as `workkey` is, and `internal/tracing`
// fills it when it starts a span.
//
// # Why it is two fixed fields and not a field bag
//
// d-401 says "log fields", which reads like an open map. An open map is a
// path for arbitrary caller-supplied text to reach a terminal, and
// TestNothingReachesTheTerminalRaw guards the ATTRIBUTE path, not this one —
// an ESC repaints a terminal and a newline forges a whole log line. Two
// validated hex fields carry everything a correlation needs and close that
// door rather than guarding it.

type traceKey struct{}

// traceFields is what rides on the context. Immutable, and both halves are
// validated before they are ever stored.
type traceFields struct{ traceID, spanID string }

// WithTrace binds a trace and span id onto ctx, so every record logged
// through a *Context method beneath it carries them.
//
// An id that is not lowercase hex of the right length is DROPPED rather than
// stored: this is the one route by which a value from outside can reach a log
// line's structure, and a trace id arrives from a remote `traceparent` header
// on the webhook edge. Both empty is the ordinary "no trace here" case and
// returns ctx untouched, so nothing allocates on the path that has no trace.
func WithTrace(ctx context.Context, traceID, spanID string) context.Context {
	if !validHex(traceID, 32) {
		traceID = ""
	}
	if !validHex(spanID, 16) {
		spanID = ""
	}
	if traceID == "" && spanID == "" {
		return ctx
	}
	return context.WithValue(ctx, traceKey{}, traceFields{traceID: traceID, spanID: spanID})
}

// TraceFromContext reports the ids bound onto ctx, if any. It exists so a
// caller that has to re-bind them (a goroutine given a detached context) can
// read them back without reaching for the tracing package.
func TraceFromContext(ctx context.Context) (traceID, spanID string) {
	tf, ok := ctx.Value(traceKey{}).(traceFields)
	if !ok {
		return "", ""
	}
	return tf.traceID, tf.spanID
}

// validHex is deliberately strict: lowercase hex only, exact length. An OTel
// id is defined that way, and anything else reaching a log line is either a
// bug here or an attacker upstream.
func validHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	var nonZero bool
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			nonZero = nonZero || c != '0'
		case c >= 'a' && c <= 'f':
			nonZero = true
		default:
			return false
		}
	}
	// An all-zero id is the OTel spelling of "invalid", and stamping it on a
	// line says a trace exists when none does.
	return nonZero
}

// attrsFor returns the record attributes for ctx, or nil when nothing is
// bound. Nil is the common case and costs one type assertion.
func attrsFor(ctx context.Context) []slog.Attr {
	tf, ok := ctx.Value(traceKey{}).(traceFields)
	if !ok {
		return nil
	}
	attrs := make([]slog.Attr, 0, 2)
	if tf.traceID != "" {
		attrs = append(attrs, slog.String("trace_id", tf.traceID))
	}
	if tf.spanID != "" {
		attrs = append(attrs, slog.String("span_id", tf.spanID))
	}
	return attrs
}
