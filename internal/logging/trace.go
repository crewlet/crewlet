package logging

import (
	"context"
	"log/slog"
)

// The trace-correlation carrier.
//
// The carrier lives here, not in the tracing package: reading the OTel span
// from ctx in the handler would put go.opentelemetry.io/otel/trace into the
// dependency graph of every package that imports this one (78) to render two
// hex strings. The carrier is self-contained, as `workkey` is, and
// `internal/tracing` fills it when it starts a span.
//
// It is two fixed fields, not a field bag: an open map is a path for
// caller-supplied text to reach a terminal (an ESC repaints it, a newline
// forges a log line) and TestNothingReachesTheTerminalRaw guards only the
// ATTRIBUTE path. Two validated hex fields close that door.

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
