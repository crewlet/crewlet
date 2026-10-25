package logging

import (
	"context"
	"log/slog"

	"github.com/crewlet/crewlet/internal/redact"
)

// redacting is the root every record passes through on its way to the
// destinations: internal/redact's pass over the message, every string
// attribute and every error, at any depth of group, and over the attributes a
// logger binds ([slog.Handler.WithAttrs]) as they are bound.
//
// # Why at the root, and not left to each call site
//
// A log line is read by whoever reads the node's stderr, its log file and
// whatever ships them, and a credential reaches one the ordinary way: a vendor
// that echoes its key in an error string, a URL with a token in it, a command
// line a setup step failed on. Many call sites already redact what they know
// can carry one, and that is the first line; this is the one place that holds
// for the call site that did not know, so the file's promise (a log line is
// redacted, see [FileMode]) is true of every line rather than of the lines
// whose author remembered.
//
// # What it covers
//
// Strings, errors (rendered as the string every handler here already renders
// an error as) and groups of them. A value of any other kind is passed as it
// is: a number, a time or a duration carries no credential, and a struct
// logged whole is a call site's choice this cannot read without changing how
// every handler renders it.
//
// # A record is never dropped
//
// Redaction cannot fail: every record is handed on, rewritten, and the
// destination's own answer is the answer. A redaction that refused a record
// it could not read would trade a credential in a log for a log with a hole
// in it, the failure [File] exists to report rather than hide.
type redacting struct{ next slog.Handler }

// Enabled is the destination's.
func (h redacting) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle hands the record on with its message and attributes redacted.
func (h redacting) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, redact.Secrets(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

// WithAttrs binds attrs redacted, so a bound attribute is redacted once rather
// than on every record.
func (h redacting) WithAttrs(attrs []slog.Attr) slog.Handler {
	return redacting{next: h.next.WithAttrs(redactAttrs(attrs))}
}

// WithGroup opens a group on the destination.
func (h redacting) WithGroup(name string) slog.Handler {
	return redacting{next: h.next.WithGroup(name)}
}

// redactAttrs is attrs, each redacted.
func redactAttrs(attrs []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = redactAttr(a)
	}
	return out
}

// redactAttr is a with its value redacted, resolving a [slog.LogValuer] first
// so a value that renders itself as a string is redacted as one.
func redactAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		v = slog.StringValue(redact.Secrets(v.String()))
	case slog.KindGroup:
		v = slog.GroupValue(redactAttrs(v.Group())...)
	case slog.KindAny:
		if err, ok := v.Any().(error); ok && err != nil {
			v = slog.StringValue(redact.Secrets(err.Error()))
		}
	}
	return slog.Attr{Key: a.Key, Value: v}
}
