package mcp

import (
	"context"
	"io"
	"log/slog"
	"sync"
)

// discardLogger is for tests that assert on behaviour rather than on logs.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// logRecorder captures structured records so a test can assert on the event
// name and attributes an operator would actually see. Several of the
// behaviours here — a surfaced stderr tail, a warning about an empty env var,
// a name collision — have no return value and exist ONLY as a log line, and a
// behaviour with no assertion is a behaviour that can be deleted silently.
type logRecorder struct {
	mu      sync.Mutex
	records []recorded
}

type recorded struct {
	Level slog.Level
	Event string
	Attrs map[string]any
}

func recorder() (*slog.Logger, *logRecorder) {
	r := &logRecorder{}
	return slog.New(r), r
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *logRecorder) Handle(_ context.Context, rec slog.Record) error {
	entry := recorded{Level: rec.Level, Event: rec.Message, Attrs: map[string]any{}}
	rec.Attrs(func(a slog.Attr) bool {
		entry.Attrs[a.Key] = a.Value.Any()
		return true
	})
	r.mu.Lock()
	r.records = append(r.records, entry)
	r.mu.Unlock()
	return nil
}

func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *logRecorder) WithGroup(string) slog.Handler      { return r }

// find returns every record with this event name.
func (r *logRecorder) find(event string) []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []recorded
	for _, rec := range r.records {
		if rec.Event == event {
			out = append(out, rec)
		}
	}
	return out
}

func (r *logRecorder) has(event string) bool { return len(r.find(event)) > 0 }
