package logging

import (
	"context"
	"log/slog"

	"github.com/crewlet/crewlet/internal/redact"
)

// redacting is the root every record passes through on its way to the
// destinations: internal/redact's pass over the message, every string
// attribute and error, at any depth of group, and over attributes a logger
// binds ([slog.Handler.WithAttrs]) as they are bound. At the root, not each
// call site: it holds for the call site that did not know a vendor echoed its
// key, so a log line is redacted ([FileMode]) in every case.
//
// It covers strings, errors and groups of them. Other kinds pass as they are:
// they carry no credential, and a struct logged whole is a call site's choice.
// A record is never dropped: refusing one would trade a credential in a log
// for a hole in it.
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
